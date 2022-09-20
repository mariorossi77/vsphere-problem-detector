package check

import (
	"context"
	"crypto/tls"
	"fmt"

	ocpv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/vsphere-problem-detector/pkg/util"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"gopkg.in/gcfg.v1"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/legacy-cloud-providers/vsphere"
)

const (
	defaultModel    = "testdata/default"
	defaultDC       = "DC0"
	defaultVMPath   = "/DC0/vm/"
	defaultHost     = "H0"
	defaultHostId   = "host-24" // Generated by vcsim
	defaultHostPath = "/DC0/host/DC0_"
	defaultUsername = "testuser"
)

type simulatedVM struct {
	name, uuid string
}

var (
	// Virtual machines generated by vSphere simulator. UUIDs look generated, but they're stable.
	defaultVMs = []simulatedVM{
		{"DC0_H0_VM0", "265104de-1472-547c-b873-6dc7883fb6cb"},
		{"DC0_H0_VM1", "12f8928d-f144-5c57-89db-dd2d0902c9fa"},
	}
)

func connectToSimulator(s *simulator.Server) (*vim25.Client, error) {
	client, err := govmomi.NewClient(context.TODO(), s.URL, true)
	if err != nil {
		return nil, err
	}
	return client.Client, nil
}

func simulatorConfig() *vsphere.VSphereConfig {
	var cfg vsphere.VSphereConfig
	// Configuration that corresponds to the simulated vSphere
	data := `[Global]
secret-name = "vsphere-creds"
secret-namespace = "kube-system"
insecure-flag = "1"

[Workspace]
server = "localhost"
datacenter = "DC0"
default-datastore = "LocalDS_0"
folder = "/DC0/vm"
resourcepool-path = "/DC0/host/DC0_H0/Resources"

[VirtualCenter "dc0"]
datacenters = "DC0"
`
	err := gcfg.ReadStringInto(&cfg, data)
	if err != nil {
		panic(err)
	}
	return &cfg
}

func setupSimulator(kubeClient *fakeKubeClient, modelDir string) (ctx *CheckContext, cleanup func(), err error) {
	model := simulator.Model{}
	err = model.Load(modelDir)
	if err != nil {
		return nil, nil, err
	}
	model.Service.TLS = new(tls.Config)

	s := model.Service.NewServer()
	client, err := connectToSimulator(s)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect to the similator: %s", err)
	}
	clusterInfo := util.NewClusterInfo()

	ctx = &CheckContext{
		Context:     context.TODO(),
		VMConfig:    simulatorConfig(),
		VMClient:    client,
		KubeClient:  kubeClient,
		ClusterInfo: clusterInfo,
	}

	ctx.VMConfig.Workspace.VCenterIP = "dc0"
	ctx.VMConfig.VirtualCenter["dc0"].User = defaultUsername

	cleanup = func() {
		s.Close()
		model.Remove()
	}
	return ctx, cleanup, nil
}

type fakeKubeClient struct {
	infrastructure *ocpv1.Infrastructure
	nodes          []*v1.Node
	storageClasses []*storagev1.StorageClass
	pvs            []*v1.PersistentVolume
}

var _ KubeClient = &fakeKubeClient{}

func (f *fakeKubeClient) GetInfrastructure(ctx context.Context) (*ocpv1.Infrastructure, error) {
	return f.infrastructure, nil
}

func (f *fakeKubeClient) ListNodes(ctx context.Context) ([]*v1.Node, error) {
	return f.nodes, nil
}

func (f *fakeKubeClient) ListStorageClasses(ctx context.Context) ([]*storagev1.StorageClass, error) {
	return f.storageClasses, nil
}

func (f *fakeKubeClient) ListPVs(ctx context.Context) ([]*v1.PersistentVolume, error) {
	return f.pvs, nil
}

func node(name string, modifiers ...func(*v1.Node)) *v1.Node {
	n := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1.NodeSpec{
			ProviderID: "",
		},
	}
	for _, modifier := range modifiers {
		modifier(n)
	}
	return n
}

func withProviderID(id string) func(*v1.Node) {
	return func(node *v1.Node) {
		node.Spec.ProviderID = id
	}
}

func defaultNodes() []*v1.Node {
	nodes := []*v1.Node{}
	for _, vm := range defaultVMs {
		node := node(vm.name, withProviderID("vsphere://"+vm.uuid))
		nodes = append(nodes, node)
	}
	return nodes
}

func infrastructure(modifiers ...func(*ocpv1.Infrastructure)) *ocpv1.Infrastructure {
	infra := &ocpv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster",
		},
		Status: ocpv1.InfrastructureStatus{
			InfrastructureName: "my-cluster-id",
		},
	}

	for _, modifier := range modifiers {
		modifier(infra)
	}
	return infra
}

func getVM(ctx *CheckContext, node *v1.Node) (*mo.VirtualMachine, error) {
	finder := find.NewFinder(ctx.VMClient, true)
	vm, err := finder.VirtualMachine(ctx.Context, defaultVMPath+node.Name)
	if err != nil {
		return nil, err
	}

	var o mo.VirtualMachine
	err = vm.Properties(ctx.Context, vm.Reference(), NodeProperties, &o)
	if err != nil {
		return nil, fmt.Errorf("failed to load VM %s: %s", node.Name, err)
	}

	return &o, nil
}

func customizeVM(ctx *CheckContext, node *v1.Node, spec *types.VirtualMachineConfigSpec) error {
	finder := find.NewFinder(ctx.VMClient, true)
	vm, err := finder.VirtualMachine(ctx.Context, defaultVMPath+node.Name)
	if err != nil {
		return err
	}

	task, err := vm.Reconfigure(ctx.Context, *spec)
	if err != nil {
		return err
	}

	err = task.Wait(ctx.Context)
	return err
}

func setHardwareVersion(ctx *CheckContext, node *v1.Node, hardwareVersion string) error {
	err := customizeVM(ctx, node, &types.VirtualMachineConfigSpec{
		ExtraConfig: []types.BaseOptionValue{
			&types.OptionValue{
				Key: "SET.config.version", Value: hardwareVersion,
			},
		}})
	return err
}

func customizeHostVersion(hostSystemId string, version string, apiVersion string) error {
	hsRef := simulator.Map.Get(types.ManagedObjectReference{
		Type:  "HostSystem",
		Value: hostSystemId,
	})
	if hsRef == nil {
		return fmt.Errorf("can't find HostSystem %s", hostSystemId)
	}

	hs := hsRef.(*simulator.HostSystem)
	hs.Config.Product.Version = version
	hs.Config.Product.ApiVersion = apiVersion
	return nil
}
