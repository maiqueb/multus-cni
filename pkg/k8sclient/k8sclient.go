// Copyright (c) 2017 Intel Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package k8sclient

import (
	"context"
	"fmt"
	"os"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"

	"github.com/containernetworking/cni/libcni"
	"github.com/containernetworking/cni/pkg/skel"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	netclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"
	netutils "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/utils"
	"gopkg.in/k8snetworkplumbingwg/multus-cni.v3/pkg/kubeletclient"
	"gopkg.in/k8snetworkplumbingwg/multus-cni.v3/pkg/logging"
	"gopkg.in/k8snetworkplumbingwg/multus-cni.v3/pkg/types"
)

const (
	resourceNameAnnot      = "k8s.v1.cni.cncf.io/resourceName"
	defaultNetAnnot        = "v1.multus-cni.io/default-network"
	networkAttachmentAnnot = "k8s.v1.cni.cncf.io/networks"
)

// NoK8sNetworkError indicates error, no network in kubernetes
type NoK8sNetworkError struct {
	message string
}

// ClientInfo contains information given from k8s client
type ClientInfo struct {
	Client           kubernetes.Interface
	NetClient        netclient.K8sCniCncfIoV1Interface
	EventBroadcaster record.EventBroadcaster
	EventRecorder    record.EventRecorder
}

// AddPod adds pod into kubernetes
func (c *ClientInfo) AddPod(pod *v1.Pod) (*v1.Pod, error) {
	return c.Client.CoreV1().Pods(pod.ObjectMeta.Namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
}

// GetPod gets pod from kubernetes
func (c *ClientInfo) GetPod(namespace, name string) (*v1.Pod, error) {
	return c.Client.CoreV1().Pods(namespace).Get(context.TODO(), name, metav1.GetOptions{})
}

// DeletePod deletes a pod from kubernetes
func (c *ClientInfo) DeletePod(namespace, name string) error {
	return c.Client.CoreV1().Pods(namespace).Delete(context.TODO(), name, metav1.DeleteOptions{})
}

// AddNetAttachDef adds net-attach-def into kubernetes
func (c *ClientInfo) AddNetAttachDef(netattach *nettypes.NetworkAttachmentDefinition) (*nettypes.NetworkAttachmentDefinition, error) {
	return c.NetClient.NetworkAttachmentDefinitions(netattach.ObjectMeta.Namespace).Create(context.TODO(), netattach, metav1.CreateOptions{})
}

// Eventf puts event into kubernetes events
func (c *ClientInfo) Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	if c != nil && c.EventRecorder != nil {
		c.EventRecorder.Eventf(object, eventtype, reason, messageFmt, args...)
	}
}

func (e *NoK8sNetworkError) Error() string { return string(e.message) }

// SetNetworkStatus sets network status into Pod annotation
func SetNetworkStatus(client *ClientInfo, k8sArgs *types.K8sArgs, netStatus []nettypes.NetworkStatus, conf *types.NetConf) error {
	podName := string(k8sArgs.K8S_POD_NAME)
	podNamespace := string(k8sArgs.K8S_POD_NAMESPACE)
	podUID := string(k8sArgs.K8S_POD_UID)

	return SetPodNetworkStatusAnnotation(client, podName, podNamespace, podUID, netStatus, conf)
}

// SetPodNetworkStatusAnnotation sets network status into Pod annotation
func SetPodNetworkStatusAnnotation(client *ClientInfo, podName string, podNamespace string, podUID string, netStatus []nettypes.NetworkStatus, conf *types.NetConf) error {
	var err error
	logging.Debugf("SetPodNetworkStatusAnnotation: %v, %v, %v", client, netStatus, conf)

	client, err = GetK8sClient(conf.Kubeconfig, client)
	if err != nil {
		return logging.Errorf("SetNetworkStatus: %v", err)
	}
	if client == nil || client.Client == nil {
		if len(conf.Delegates) == 0 {
			// No available kube client and no delegates, we can't do anything
			return logging.Errorf("SetNetworkStatus: must have either Kubernetes config or delegates")
		}
		logging.Debugf("SetPodNetworkStatusAnnotation: kube client info is not defined, skip network status setup")
		return nil
	}

	pod, err := client.GetPod(podNamespace, podName)
	if err != nil {
		return logging.Errorf("SetPodNetworkStatusAnnotation: failed to query the pod %v in out of cluster comm: %v", podName, err)
	}

	if podUID != "" && string(pod.UID) != podUID && !IsStaticPod(pod) {
		return logging.Errorf("SetNetworkStatus: expected pod %s/%s UID %q but got %q from Kube API", podNamespace, podName, podUID, pod.UID)
	}

	if netStatus != nil {
		err = netutils.SetNetworkStatus(client.Client, pod, netStatus)
		if err != nil {
			return logging.Errorf("SetPodNetworkStatusAnnotation: failed to update the pod %v in out of cluster comm: %v", podName, err)
		}
	}

	return nil
}

// GetKubernetesDelegate returns the delegate configuration for a given `NetworkSelectionElement` on a pod.
func GetKubernetesDelegate(client *ClientInfo, net *types.NetworkSelectionElement, confdir string, pod *v1.Pod, resourceMap map[string]*types.ResourceInfo) (*types.DelegateNetConf, map[string]*types.ResourceInfo, error) {

	logging.Debugf("GetKubernetesDelegate: %v, %v, %s, %v, %v", client, net, confdir, pod, resourceMap)
	customResource, err := client.NetClient.NetworkAttachmentDefinitions(net.Namespace).Get(context.TODO(), net.Name, metav1.GetOptions{})
	if err != nil {
		errMsg := fmt.Sprintf("cannot find a network-attachment-definition (%s) in namespace (%s): %v", net.Name, net.Namespace, err)
		if client != nil {
			client.Eventf(pod, v1.EventTypeWarning, "NoNetworkFound", errMsg)
		}
		return nil, resourceMap, logging.Errorf("GetKubernetesDelegate: " + errMsg)
	}

	logging.Debugf("GetKubernetesDelegate: found custom resource: %+v", *customResource)
	// Get resourceName annotation from NetworkAttachmentDefinition
	deviceID := ""
	resourceName, ok := customResource.GetAnnotations()[resourceNameAnnot]
	if ok && pod.Name != "" && pod.Namespace != "" {
		// ResourceName annotation is found; try to get device info from resourceMap
		logging.Debugf("GetKubernetesDelegate: found resourceName annotation : %s", resourceName)

		if resourceMap == nil {
			ck, err := kubeletclient.GetResourceClient("")
			if err != nil {
				return nil, resourceMap, logging.Errorf("GetKubernetesDelegate: failed to get a ResourceClient instance: %v", err)
			}
			resourceMap, err = ck.GetPodResourceMap(pod)
			if err != nil {
				return nil, resourceMap, logging.Errorf("GetKubernetesDelegate: failed to get resourceMap from ResourceClient: %v", err)
			}
			logging.Debugf("GetKubernetesDelegate: resourceMap instance: %+v", resourceMap)
		}

		entry, ok := resourceMap[resourceName]
		if ok {
			if idCount := len(entry.DeviceIDs); idCount > 0 && idCount > entry.Index {
				deviceID = entry.DeviceIDs[entry.Index]
				logging.Debugf("GetKubernetesDelegate: podName: %s deviceID: %s", pod.Name, deviceID)
				entry.Index++ // increment Index for next delegate
			}
		}
	}
	logging.Debugf("GetKubernetesDelegate: about to read the custom resource CNI config for %s", customResource.Name)
	configBytes, err := netutils.GetCNIConfig(customResource, confdir)
	if err != nil {
		return nil, resourceMap, err
	}

	logging.Debugf("GetKubernetesDelegate: got all tha tasty bytes from the custom resource. they are: %s", string(configBytes))
	delegate, err := types.LoadDelegateNetConf(configBytes, net, deviceID, resourceName)
	if err != nil {
		return nil, resourceMap, err
	}
	if delegate != nil {
		logging.Debugf("GetKubernetesDelegate: loaded those delegates: %+v", *delegate)
	} else {
		logging.Debugf("GetKubernetesDelegate: loaded those delegates: %+v", delegate)
	}
	return delegate, resourceMap, nil
}

// GetK8sArgs gets k8s related args from CNI args
func GetK8sArgs(args *skel.CmdArgs) (*types.K8sArgs, error) {
	k8sArgs := &types.K8sArgs{}

	logging.Debugf("GetK8sArgs: %v", args)
	err := cnitypes.LoadArgs(args.Args, k8sArgs)
	if err != nil {
		return nil, err
	}

	return k8sArgs, nil
}

// TryLoadPodDelegates attempts to load Kubernetes-defined delegates and add them to the Multus config.
// Returns the number of Kubernetes-defined delegates added or an error.
func TryLoadPodDelegates(pod *v1.Pod, conf *types.NetConf, clientInfo *ClientInfo, resourceMap map[string]*types.ResourceInfo) (int, *ClientInfo, error) {
	var err error

	logging.Debugf("TryLoadPodDelegates: %v, %v, %v", pod, conf, clientInfo)
	clientInfo, err = GetK8sClient(conf.Kubeconfig, clientInfo)
	if err != nil {
		return 0, nil, err
	}

	if clientInfo == nil {
		if len(conf.Delegates) == 0 {
			// No available kube client and no delegates, we can't do anything
			return 0, nil, logging.Errorf("TryLoadPodDelegates: must have either Kubernetes config or delegates")
		}
		return 0, nil, nil
	}

	delegate, err := tryLoadK8sPodDefaultNetwork(clientInfo, pod, conf)
	if err != nil {
		return 0, nil, logging.Errorf("TryLoadPodDelegates: error in loading K8s cluster default network from pod annotation: %v", err)
	}
	if delegate != nil {
		logging.Debugf("TryLoadPodDelegates: Overwrite the cluster default network with %v from pod annotations", delegate)

		conf.Delegates[0] = delegate
	}

	networks, err := GetPodNetwork(pod)
	if networks != nil {
		delegates, err := GetNetworkDelegates(clientInfo, pod, networks, conf, resourceMap)

		if err != nil {
			if _, ok := err.(*NoK8sNetworkError); ok {
				return 0, clientInfo, nil
			}
			return 0, nil, logging.Errorf("TryLoadPodDelegates: error in getting k8s network for pod: %v", err)
		}

		if err = conf.AddDelegates(delegates); err != nil {
			return 0, nil, err
		}

		// Check gatewayRequest is configured in delegates
		// and mark its config if gateway filter is required
		isGatewayConfigured := false
		for _, delegate := range conf.Delegates {
			if delegate.GatewayRequest != nil {
				isGatewayConfigured = true
				break
			}
		}

		if isGatewayConfigured == true {
			err = types.CheckGatewayConfig(conf.Delegates)
			if err != nil {
				return 0, nil, err
			}
		}

		return len(delegates), clientInfo, err
	}

	if _, ok := err.(*NoK8sNetworkError); ok {
		return 0, clientInfo, nil
	}
	return 0, clientInfo, err
}

// InClusterK8sClient returns the `k8s.ClientInfo` struct to use to connect to
// the k8s API.
func InClusterK8sClient() (*ClientInfo, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}

	logging.Debugf("InClusterK8sClient: in cluster config: %+v", config)
	return NewClientInfo(config)
}

// GetK8sClient gets client info from kubeconfig
func GetK8sClient(kubeconfig string, kubeClient *ClientInfo) (*ClientInfo, error) {
	logging.Debugf("GetK8sClient: %s, %v", kubeconfig, kubeClient)
	// If we get a valid kubeClient (eg from testcases) just return that
	// one.
	if kubeClient != nil {
		return kubeClient, nil
	}

	var err error
	var config *rest.Config

	// Otherwise try to create a kubeClient from a given kubeConfig
	if kubeconfig != "" {
		// uses the current context in kubeconfig
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, logging.Errorf("GetK8sClient: failed to get context for the kubeconfig %v: %v", kubeconfig, err)
		}
	} else if os.Getenv("KUBERNETES_SERVICE_HOST") != "" && os.Getenv("KUBERNETES_SERVICE_PORT") != "" {
		// Try in-cluster config where multus might be running in a kubernetes pod
		config, err = rest.InClusterConfig()
		if err != nil {
			return nil, logging.Errorf("GetK8sClient: failed to get context for in-cluster kube config: %v", err)
		}
	} else {
		// No kubernetes config; assume we shouldn't talk to Kube at all
		return nil, nil
	}

	// Specify that we use gRPC
	config.AcceptContentTypes = "application/vnd.kubernetes.protobuf,application/json"
	config.ContentType = "application/vnd.kubernetes.protobuf"
	// Set the config timeout to one minute.
	config.Timeout = time.Minute

	return NewClientInfo(config)
}

// NewClientInfo returns a `ClientInfo` from a configuration created from an
// existing kubeconfig file.
func NewClientInfo(config *rest.Config) (*ClientInfo, error) {
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	netclient, err := netclient.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	broadcaster := record.NewBroadcaster()
	broadcaster.StartLogging(klog.Infof)
	broadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: client.CoreV1().Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "multus"})
	return &ClientInfo{
		Client:           client,
		NetClient:        netclient,
		EventBroadcaster: broadcaster,
		EventRecorder:    recorder,
	}, nil
}

// GetPodNetwork gets net-attach-def annotation from pod
func GetPodNetwork(pod *v1.Pod) ([]*types.NetworkSelectionElement, error) {
	logging.Debugf("GetPodNetwork: %v", pod)

	netAnnot := pod.Annotations[networkAttachmentAnnot]
	defaultNamespace := pod.ObjectMeta.Namespace

	if len(netAnnot) == 0 {
		return nil, &NoK8sNetworkError{"no kubernetes network found"}
	}

	networks, err := types.ParsePodNetworkAnnotation(netAnnot, defaultNamespace)
	if err != nil {
		return nil, err
	}
	return networks, nil
}

// GetNetworkDelegates returns delegatenetconf from net-attach-def annotation in pod
func GetNetworkDelegates(k8sclient *ClientInfo, pod *v1.Pod, networks []*types.NetworkSelectionElement, conf *types.NetConf, resourceMap map[string]*types.ResourceInfo) ([]*types.DelegateNetConf, error) {
	logging.Debugf("GetNetworkDelegates: %v, %v, %v, %v, %v", k8sclient, pod, networks, conf, resourceMap)

	// Read all network objects referenced by 'networks'
	var delegates []*types.DelegateNetConf
	defaultNamespace := pod.ObjectMeta.Namespace

	for _, net := range networks {

		// The pods namespace (stored as defaultNamespace, does not equal the annotation's target namespace in net.Namespace)
		// In the case that this is a mismatch when namespaceisolation is enabled, this should be an error.
		if conf.NamespaceIsolation {
			if defaultNamespace != net.Namespace {
				// We allow exceptions based on the specified list of non-isolated namespaces (and/or "default" namespace, by default)
				if !isValidNamespaceReference(net.Namespace, conf.NonIsolatedNamespaces) {
					return nil, logging.Errorf("GetNetworkDelegates: namespace isolation enabled, annotation violates permission, pod is in namespace %v but refers to target namespace %v", defaultNamespace, net.Namespace)
				}
			}
		}

		delegate, updatedResourceMap, err := GetKubernetesDelegate(k8sclient, net, conf.ConfDir, pod, resourceMap)
		if err != nil {
			return nil, logging.Errorf("GetNetworkDelegates: failed getting the delegate: %v", err)
		}
		delegates = append(delegates, delegate)
		resourceMap = updatedResourceMap
	}

	return delegates, nil
}

func isValidNamespaceReference(targetns string, allowednamespaces []string) bool {
	for _, eachns := range allowednamespaces {
		if eachns == targetns {
			return true
		}
	}
	return false
}

func getNetDelegate(client *ClientInfo, pod *v1.Pod, netname, confdir, namespace string, resourceMap map[string]*types.ResourceInfo) (*types.DelegateNetConf, map[string]*types.ResourceInfo, error) {
	logging.Debugf("getNetDelegate: %v, %v, %v, %s", client, netname, confdir, namespace)
	// option1) search CRD object for the network
	net := &types.NetworkSelectionElement{
		Name:      netname,
		Namespace: namespace,
	}
	delegate, resourceMap, err := GetKubernetesDelegate(client, net, confdir, pod, resourceMap)
	if err == nil {
		return delegate, resourceMap, nil
	}

	// option2) search CNI json config file
	var configBytes []byte
	configBytes, err = netutils.GetCNIConfigFromFile(netname, confdir)
	if err == nil {
		delegate, err := types.LoadDelegateNetConf(configBytes, nil, "", "")
		if err != nil {
			return nil, resourceMap, err
		}
		return delegate, resourceMap, nil
	}

	// option3) search directory
	fInfo, err := os.Stat(netname)
	if err == nil {
		if fInfo.IsDir() {
			files, err := libcni.ConfFiles(netname, []string{".conf", ".conflist"})
			if err != nil {
				return nil, resourceMap, err
			}
			if len(files) > 0 {
				var configBytes []byte
				configBytes, err = netutils.GetCNIConfigFromFile("", netname)
				if err == nil {
					delegate, err := types.LoadDelegateNetConf(configBytes, nil, "", "")
					if err != nil {
						return nil, resourceMap, err
					}
					return delegate, resourceMap, nil
				}
				return nil, resourceMap, err
			}
		}
	}
	return nil, resourceMap, logging.Errorf("getNetDelegate: cannot find network: %v", netname)
}

// GetDefaultNetworks parses 'defaultNetwork' config, gets network json and put it into netconf.Delegates.
func GetDefaultNetworks(pod *v1.Pod, conf *types.NetConf, kubeClient *ClientInfo, resourceMap map[string]*types.ResourceInfo) (map[string]*types.ResourceInfo, error) {
	logging.Debugf("GetDefaultNetworks: %v, %v, %v, %v", pod, conf, kubeClient, resourceMap)
	var delegates []*types.DelegateNetConf

	kubeClient, err := GetK8sClient(conf.Kubeconfig, kubeClient)
	if err != nil {
		return resourceMap, err
	}
	if kubeClient == nil {
		if len(conf.Delegates) == 0 {
			// No available kube client and no delegates, we can't do anything
			return resourceMap, logging.Errorf("GetDefaultNetworks: must have either Kubernetes config or delegates")
		}
		return resourceMap, nil
	}

	delegate, resourceMap, err := getNetDelegate(kubeClient, pod, conf.ClusterNetwork, conf.ConfDir, conf.MultusNamespace, resourceMap)

	if err != nil {
		return resourceMap, logging.Errorf("GetDefaultNetworks: failed to get clusterNetwork %s in namespace %s", conf.ClusterNetwork, conf.MultusNamespace)
	}
	delegate.MasterPlugin = true
	delegates = append(delegates, delegate)

	// Pod in kube-system namespace does not have default network for now.
	if !types.CheckSystemNamespaces(pod.ObjectMeta.Namespace, conf.SystemNamespaces) {
		for _, netname := range conf.DefaultNetworks {
			delegate, resourceMap, err := getNetDelegate(kubeClient, pod, netname, conf.ConfDir, conf.MultusNamespace, resourceMap)
			if err != nil {
				return resourceMap, err
			}
			delegates = append(delegates, delegate)
		}
	}

	if err = conf.AddDelegates(delegates); err != nil {
		return resourceMap, err
	}

	return resourceMap, nil
}

// tryLoadK8sPodDefaultNetwork get pod default network from annotations
func tryLoadK8sPodDefaultNetwork(kubeClient *ClientInfo, pod *v1.Pod, conf *types.NetConf) (*types.DelegateNetConf, error) {
	var netAnnot string
	logging.Debugf("tryLoadK8sPodDefaultNetwork: %v, %v, %v", kubeClient, pod, conf)

	netAnnot, ok := pod.Annotations[defaultNetAnnot]
	if !ok {
		logging.Debugf("tryLoadK8sPodDefaultNetwork: Pod default network annotation is not defined")
		return nil, nil
	}

	// The CRD object of default network should only be defined in multusNamespace
	networks, err := types.ParsePodNetworkAnnotation(netAnnot, conf.MultusNamespace)
	if err != nil {
		return nil, logging.Errorf("tryLoadK8sPodDefaultNetwork: failed to parse CRD object: %v", err)
	}
	if len(networks) > 1 {
		return nil, logging.Errorf("tryLoadK8sPodDefaultNetwork: more than one default network is specified: %s", netAnnot)
	}

	delegate, _, err := GetKubernetesDelegate(kubeClient, networks[0], conf.ConfDir, pod, nil)
	if err != nil {
		return nil, logging.Errorf("tryLoadK8sPodDefaultNetwork: failed getting the delegate: %v", err)
	}
	delegate.MasterPlugin = true

	return delegate, nil
}

// ConfigSourceAnnotationKey specifies kubernetes annotation, defined in k8s.io/kubernetes/pkg/kubelet/types
const ConfigSourceAnnotationKey = "kubernetes.io/config.source"

// IsStaticPod returns true if the pod is static pod.
func IsStaticPod(pod *v1.Pod) bool {
	if pod.Annotations != nil {
		if source, ok := pod.Annotations[ConfigSourceAnnotationKey]; ok == true {
			return source != "api"
		}
	}
	return false
}
