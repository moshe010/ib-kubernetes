package daemon

import (
	"encoding/json"
	"net"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/Mellanox/ib-kubernetes/pkg/guid"
	k8sClient "github.com/Mellanox/ib-kubernetes/pkg/k8s-client"
	"github.com/Mellanox/ib-kubernetes/pkg/sm"
	"github.com/Mellanox/ib-kubernetes/pkg/sm/plugins"
	"github.com/Mellanox/ib-kubernetes/pkg/utils"
	"github.com/Mellanox/ib-kubernetes/pkg/watcher"
	resEvenHandler "github.com/Mellanox/ib-kubernetes/pkg/watcher/resource-event-handler"

	"github.com/golang/glog"
	v1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	netAttUtils "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/utils"
	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

type Daemon interface {
	// Run run listener for k8s pod events.
	Run()
}

type daemon struct {
	watcher    watcher.Watcher
	kubeClient k8sClient.Client
	guidPool   guid.GuidPool
	smClient   plugins.SubnetManagerClient
}

// NewDaemon initializes the need components including k8s client, subnet manager client plugins, and guid pool.
// It returns error in case of failure.
func NewDaemon() (Daemon, error) {
	glog.Info("daemon NewDaemon():")
	podEventHandler := resEvenHandler.NewPodEventHandler()
	client, err := k8sClient.NewK8sClient()

	if err != nil {
		glog.Error(err)
		return nil, err
	}

	guidPool, err := guid.NewGuidPool("02:00:00:00:00:00:00:00", "02:00:00:00:00:00:FF:FF",
		client)
	if err != nil {
		glog.Error(err)
		return nil, err
	}

	if err = guidPool.InitPool(); err != nil {
		glog.Error(err)
		return nil, err
	}

	pluginLoader := sm.NewPluginLoader()
	getSmClientFunc, err := pluginLoader.LoadPlugin("/plugins/ufm.so", sm.InitializePluginFunc)
	if err != nil {
		return nil, err
	}

	smClient, err := getSmClientFunc([]byte(`{
    "username": "admin",
    "password": "123456",
    "address": "r-dcs5-02"
}`))
	if err != nil {
		return nil, err
	}

	if err := smClient.Validate(); err != nil {
		return nil, err
	}

	podWatcher := watcher.NewWatcher(podEventHandler, client)
	return &daemon{
		watcher:    podWatcher,
		kubeClient: client,
		guidPool:   guidPool,
		smClient:   smClient}, nil
}

func (d *daemon) Run() {
	glog.Info("daemon Run():")
	go wait.Forever(d.AddPeriodicUpdate, 2*time.Second)
	go wait.Forever(d.DeletePeriodicUpdate, 2*time.Second)
	glog.Info("watcher Run():")
	d.watcher.Run()
}

func (d *daemon) AddPeriodicUpdate() {
	glog.Info("AddPeriodicUpdate():")
	addMap, _ := d.watcher.GetHandler().GetResults()
	addMap.Lock()
	podNetworksMap := map[types.UID][]*v1.NetworkSelectionElement{}
	for networkName, podsInterface := range addMap.Items {
		glog.Infof("AddPeriodicUpdate(): networkName %s", networkName)
		pods, ok := podsInterface.([]*kapi.Pod)
		if !ok {
			glog.Errorf("AddPeriodicUpdate(): invalid value for add map networks expected pods array \"[]*kubernetes.Pod\", found %T", podsInterface)
			continue
		}

		if len(pods) == 0 {
			continue
		}

		networkNamespace := pods[0].Namespace
		netAttInfo, err := d.kubeClient.GetNetworkAttachmentDefinition(networkNamespace, networkName)
		if err != nil {
			glog.Warningf("AddPeriodicUpdate(): failed to get networkName attachment %s with error: %v", networkName, err)
			// skip failed networks
			continue
		}

		glog.V(3).Infof("AddPeriodicUpdate(): networkName attachment %v", netAttInfo)
		networkSpec := make(map[string]interface{})
		err = json.Unmarshal([]byte(netAttInfo.Spec.Config), &networkSpec)
		if err != nil {
			glog.Warningf("AddPeriodicUpdate(): failed to parse networkName attachment %s with error: %v", networkName, err)
			// skip failed networks
			continue
		}
		glog.V(3).Infof("AddPeriodicUpdate(): networkName attachment spec %+v", networkSpec)

		ibCniSpec, err := utils.IsIbSriovCniInNetwork(networkSpec)
		if err != nil {
			glog.Warningf("AddPeriodicUpdate(): %v", err)
			// skip failed network
			continue
		}
		glog.V(3).Infof("AddPeriodicUpdate(): CNI spec %+v", ibCniSpec)

		var guidList []net.HardwareAddr
		var passedPods []*kapi.Pod
		for _, pod := range pods {
			glog.Infof("AddPeriodicUpdate(): pod namespace %s name %s", pod.Namespace, pod.Name)
			ibAnnotation, err := utils.ParseInfiniBandAnnotation(pod)
			if err == nil {
				if utils.IsPodNetworkConfiguredWithInfiniBand(ibAnnotation, networkName) {
					continue
				}
			}
			networks, ok := podNetworksMap[pod.UID]
			if !ok {
				networks, err = netAttUtils.ParsePodNetworkAnnotation(pod)
				if err != nil {
					glog.Errorf("AddPeriodicUpdate(): failed to read pod networkName annotations pod namespace %s name %s, with error: %v",
						pod.Namespace, pod.Name, err)
					continue
				}

				podNetworksMap[pod.UID] = networks
			}
			network, err := utils.GetPodNetwork(networks, networkName)
			if err != nil {
				glog.Errorf("AddPeriodicUpdate(): failed to get pod networkName spec %s with error: %v",
					networkName, err)
				// skip failed pod
				continue
			}

			var guidAddr net.HardwareAddr
			allocatedGuid, err := utils.GetPodNetworkGuid(network)
			if err == nil {
				// User allocated guid manually
				if err = d.guidPool.AllocateGUID(string(pod.UID)+networkName, allocatedGuid); err != nil {
					glog.Errorf("AddPeriodicUpdate(): %v", err)
					continue
				}
				guidAddr, err = net.ParseMAC(allocatedGuid)
				if err != nil {
					glog.Errorf("AddPeriodicUpdate(): failed to parse user allocated guid %s with error: %v",
						allocatedGuid, err)
					continue
				}
			} else {
				allocatedGuid, err = d.guidPool.GenerateGUID("")
				if err != nil {
					glog.Error(err)
					continue
				}

				if err = utils.SetPodNetworkGuid(network, allocatedGuid); err != nil {
					glog.Errorf("AddPeriodicUpdate(): failed to set pod networkName annotation guid with error: %v ", err)
					continue
				}

				guidAddr, _ = net.ParseMAC(allocatedGuid)
			}
			netAnnotations, err := json.Marshal(networks)
			if err != nil {
				glog.Warningf("AddPeriodicUpdate(): failed to dump networks %+v of pod into json with error: %v",
					networks, err)
				continue
			}
			pod.Annotations[v1.NetworkAttachmentAnnot] = string(netAnnotations)

			guidList = append(guidList, guidAddr)
			passedPods = append(passedPods, pod)
		}

		if ibCniSpec.PKey != "" && len(guidList) != 0 {
			pKey, err := utils.ParsePKey(ibCniSpec.PKey)
			if err != nil {
				glog.Errorf("AddPeriodicUpdate(): failed to parse PKey %s with error: %v", ibCniSpec.PKey, err)
				continue
			}

			if err = d.smClient.AddGuidsToPKey(pKey, guidList); err != nil {
				glog.Errorf("AddPeriodicUpdate(): failed to config pKey with subnet manager %s with error: %v",
					d.smClient.Name(), err)
				continue
			}
		}

		// Update annotations for passed pods
		finalPassCounter := 0
		for index, pod := range passedPods {
			ibAnnotation, err := utils.ParseInfiniBandAnnotation(pod)
			if err != nil {
				ibAnnotation = map[string]string{networkName: utils.ConfiguredInfiniBandPod}
			}
			ibAnnotationsData, _ := json.Marshal(ibAnnotation)

			pod.Annotations[utils.InfiniBandAnnotation] = string(ibAnnotationsData)
			if err := d.kubeClient.SetAnnotationsOnPod(pod, pod.Annotations); err != nil {
				if !strings.Contains(strings.ToLower(err.Error()), "not found") {
					glog.Errorf("AddPeriodicUpdate(): failed to update pod annotations with err: %v", err)
					continue
				}

				if err = d.guidPool.ReleaseGUID(guidList[index].String()); err != nil {
					glog.Warningf("AddPeriodicUpdate(): failed to release guid \"%s\" from removed pod \"%s\""+
						" in namespace \"%s\" with error: %v", guidList[index].String(), pod.Name, pod.Namespace, err)
				}
			}

			finalPassCounter++
		}
		if len(pods) == len(passedPods) && len(passedPods) == finalPassCounter {
			addMap.UnSafeRemove(networkName)
		}
	}
	glog.Info("AddPeriodicUpdate(): finished")
	addMap.Unlock()
}

func (d *daemon) DeletePeriodicUpdate() {
	glog.Info("DeletePeriodicUpdate():")
	_, deleteMap := d.watcher.GetHandler().GetResults()
	deleteMap.Lock()
	for networkName, podsInterface := range deleteMap.Items {
		glog.Infof("DeletePeriodicUpdate(): networkName %s", networkName)
		pods, ok := podsInterface.([]*kapi.Pod)
		if !ok {
			glog.Errorf("DeletePeriodicUpdate(): invalid value for add map networks expected pods array \"[]*kubernetes.Pod\", found %T", podsInterface)
			continue
		}

		if len(pods) == 0 {
			continue
		}

		networkNamespace := pods[0].Namespace
		netAttInfo, err := d.kubeClient.GetNetworkAttachmentDefinition(networkNamespace, networkName)
		if err != nil {
			glog.Warningf("DeletePeriodicUpdate(): failed to get networkName attachment %s with error: %v", networkName, err)
			// skip failed networks
			continue
		}
		glog.V(3).Infof("DeletePeriodicUpdate(): networkName attachment %v", netAttInfo)

		networkSpec := make(map[string]interface{})
		err = json.Unmarshal([]byte(netAttInfo.Spec.Config), &networkSpec)
		if err != nil {
			glog.Warningf("DeletePeriodicUpdate(): failed to parse networkName attachment %s with error: %v", networkName, err)
			// skip failed networks
			continue
		}
		glog.V(3).Infof("DeletePeriodicUpdate(): networkName attachment spec %+v", networkSpec)

		ibCniSpec, err := utils.IsIbSriovCniInNetwork(networkSpec)
		if err != nil {
			glog.Warningf("DeletePeriodicUpdate(): %v", err)
			// skip failed networks
			continue
		}
		glog.V(3).Infof("DeletePeriodicUpdate(): CNI spec %+v", ibCniSpec)

		var guidList []net.HardwareAddr
		for _, pod := range pods {
			glog.Infof("DeletePeriodicUpdate(): pod namespace %s name %s", pod.Namespace, pod.Name)
			ibAnnotation, netErr := utils.ParseInfiniBandAnnotation(pod)
			if netErr == nil {
				if !utils.IsPodNetworkConfiguredWithInfiniBand(ibAnnotation, networkName) {
					continue
				}
			}
			networks, netErr := netAttUtils.ParsePodNetworkAnnotation(pod)
			if err != nil {
				glog.Errorf("DeletePeriodicUpdate(): failed to read pod networkName annotations pod namespace %s name %s, with error: %v",
					pod.Namespace, pod.Name, netErr)
				continue
			}

			network, netErr := utils.GetPodNetwork(networks, networkName)
			if netErr != nil {
				glog.Errorf("DeletePeriodicUpdate(): failed to get pod networkName spec %s with error: %v",
					networkName, netErr)
				// skip failed pod
				continue
			}

			allocatedGuid, netErr := utils.GetPodNetworkGuid(network)
			if err != nil {
				glog.Errorf("DeletePeriodicUpdate(): %v", netErr)
				continue
			}

			guidAddr, _ := net.ParseMAC(allocatedGuid)
			guidList = append(guidList, guidAddr)
		}

		if ibCniSpec.PKey != "" && len(guidList) != 0 {
			pKey, pkeyErr := utils.ParsePKey(ibCniSpec.PKey)
			if err != nil {
				glog.Errorf("DeletePeriodicUpdate(): failed to parse PKey %s with error: %v", ibCniSpec.PKey, pkeyErr)
				continue
			}

			if pkeyErr = d.smClient.RemoveGuidsFromPKey(pKey, guidList); err != nil {
				glog.Errorf("DeletePeriodicUpdate(): failed to config pKey with subnet manager %s with error: %v",
					d.smClient.Name(), pkeyErr)
				continue
			}
		}

		for _, guidAddr := range guidList {
			if err = d.guidPool.ReleaseGUID(guidAddr.String()); err != nil {
				glog.Error(err)
				continue
			}
		}
		deleteMap.UnSafeRemove(networkName)
	}

	glog.Info("DeletePeriodicUpdate(): finished")
	deleteMap.Unlock()
}
