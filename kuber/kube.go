package kuber

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/MagalixCorp/magalix-agent/utils"
	"github.com/MagalixTechnologies/log-go"
	"github.com/reconquest/karma-go"
	"golang.org/x/sync/errgroup"
	"k8s.io/api/apps/v1"
	kbeta2 "k8s.io/api/apps/v1beta2"
	kbeta1 "k8s.io/api/batch/v1beta1"
	kv1 "k8s.io/api/core/v1"
	kmeta "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	beta2client "k8s.io/client-go/kubernetes/typed/apps/v1beta2"
	kapps "k8s.io/client-go/kubernetes/typed/apps/v1beta2"
	batch "k8s.io/client-go/kubernetes/typed/batch/v1beta1"
	kcore "k8s.io/client-go/kubernetes/typed/core/v1"
	krest "k8s.io/client-go/rest"
	certutil "k8s.io/client-go/util/cert"

	"github.com/MagalixCorp/magalix-agent/client"
	"github.com/MagalixCorp/magalix-agent/proto"
)

const (
	milliCore   = 1000
	maskedValue = "**MASKED**"
)

// Kube kube struct
type Kube struct {
	Clientset     *kubernetes.Clientset
	ClientV1Beta2 *beta2client.AppsV1beta2Client
	ClientBatch   *batch.BatchV1beta1Client

	core   kcore.CoreV1Interface
	apps   kapps.AppsV1beta2Interface
	batch  batch.BatchV1beta1Interface
	config *krest.Config
	logger *log.Logger
}

// RequestLimit request limit
type RequestLimit struct {
	CPU    *int64
	Memory *int64
}

// ContainerResources container resources
type ContainerResourcesRequirements struct {
	Name     string
	Requests RequestLimit
	Limits   RequestLimit
}

// TotalResources service resources and replicas
type TotalResources struct {
	Replicas   *int
	Containers []ContainerResourcesRequirements
}

type Resource struct {
	Namespace      string
	Name           string
	Kind           string
	Annotations    map[string]string
	ReplicasStatus proto.ReplicasStatus
	Containers     []kv1.Container
	PodRegexp      *regexp.Regexp
}

type RawResources struct {
	PodList        *kv1.PodList
	LimitRangeList *kv1.LimitRangeList

	CronJobList *kbeta1.CronJobList

	DeploymentList  *kbeta2.DeploymentList
	StatefulSetList *kbeta2.StatefulSetList
	DaemonSetList   *kbeta2.DaemonSetList
	ReplicaSetList  *kbeta2.ReplicaSetList
}

func InitKubernetes(
	args map[string]interface{},
	client *client.Client,
) (*Kube, error) {
	var config *krest.Config
	var err error

	if args["--kube-incluster"].(bool) {
		client.Infof(nil, "initializing kubernetes incluster config")

		config, err = krest.InClusterConfig()
		if err != nil {
			return nil, karma.Format(
				err,
				"unable to get incluster config",
			)
		}

	} else {
		client.Infof(
			nil,
			"initializing kubernetes user-defined config",
		)

		token, _ := args["--kube-token"].(string)
		if token == "" {
			token = os.Getenv("KUBE_TOKEN")
		}

		config = &krest.Config{}
		config.ContentType = kruntime.ContentTypeJSON
		config.APIPath = "/api"
		config.Host = args["--kube-url"].(string)
		config.BearerToken = token

		{
			tlsClientConfig := krest.TLSClientConfig{}
			rootCAFile, ok := args["--kube-root-ca-cert"].(string)
			if ok {
				if _, err := certutil.NewPool(rootCAFile); err != nil {
					fmt.Printf("Expected to load root CA config from %s, but got err: %v", rootCAFile, err)
				} else {
					tlsClientConfig.CAFile = rootCAFile
				}
				config.TLSClientConfig = tlsClientConfig
			}
		}

		if args["--kube-insecure"].(bool) {
			config.Insecure = true
		}
	}

	config.Timeout = utils.MustParseDuration(args, "--kube-timeout")

	client.Debugf(
		karma.
			Describe("url", config.Host).
			Describe("token", config.BearerToken).
			Describe("insecure", config.Insecure),
		"initializing kubernetes Clientset",
	)

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, karma.Format(
			err,
			"unable to create Clientset",
		)
	}

	clientV1Beta2, err := beta2client.NewForConfig(config)
	if err != nil {
		return nil, karma.Format(
			err,
			"unable to create ClientV1Beta2",
		)
	}

	clientV1Beta1, err := batch.NewForConfig(config)
	if err != nil {
		return nil, karma.Format(
			err,
			"unable to create batch clientV1Beta1",
		)
	}

	kube := &Kube{
		Clientset:     clientset,
		ClientV1Beta2: clientV1Beta2,
		core:          clientset.CoreV1(),
		apps:          clientset.AppsV1beta2(),
		batch:         clientV1Beta1,
		config:        config,
		logger:        client.Logger,
	}

	return kube, nil
}

// GetNodes get kubernetes nodes
func (kube *Kube) GetNodes() (*kv1.NodeList, error) {
	kube.logger.Debugf(nil, "{kubernetes} retrieving list of nodes")
	nodes, err := kube.core.Nodes().List(kmeta.ListOptions{})
	if err != nil {
		return nil, karma.Format(
			err,
			"unable to retrieve nodes from all namespaces",
		)
	}

	return nodes, nil
}

func (kube *Kube) GetResources() (
	pods []kv1.Pod,
	limitRanges []kv1.LimitRange,
	resources []Resource,
	rawResources map[string]interface{},
	err error,
) {
	rawResources = map[string]interface{}{}

	m := sync.Mutex{}
	group := errgroup.Group{}

	group.Go(func() error {
		controllers, err := kube.GetReplicationControllers()
		if err != nil {
			return karma.Format(
				err,
				"unable to get replication controllers",
			)
		}

		if controllers != nil {
			m.Lock()
			defer m.Unlock()

			rawResources["controllers"] = controllers

			for _, controller := range controllers.Items {
				resources = append(resources, Resource{
					Kind:        "ReplicationController",
					Annotations: controller.Annotations,
					Namespace:   controller.Namespace,
					Name:        controller.Name,
					Containers:  controller.Spec.Template.Spec.Containers,
					PodRegexp: regexp.MustCompile(
						fmt.Sprintf(
							"^%s-[^-]+$",
							regexp.QuoteMeta(controller.Name),
						),
					),
					ReplicasStatus: proto.ReplicasStatus{
						Desired:   controller.Spec.Replicas,
						Current:   newInt32Pointer(controller.Status.Replicas),
						Ready:     newInt32Pointer(controller.Status.ReadyReplicas),
						Available: newInt32Pointer(controller.Status.AvailableReplicas),
					},
				})
			}
		}
		return nil
	})

	group.Go(func() error {
		podList, err := kube.GetPods()
		if err != nil {
			return karma.Format(
				err,
				"unable to get pods",
			)
		}

		if podList != nil {
			pods = podList.Items

			m.Lock()
			defer m.Unlock()

			rawResources["pods"] = podList

			for _, pod := range pods {
				if len(pod.OwnerReferences) > 0 {
					continue
				}
				resources = append(resources, Resource{
					Kind:        "OrphanPod",
					Annotations: pod.Annotations,
					Namespace:   pod.Namespace,
					Name:        pod.Name,
					Containers:  pod.Spec.Containers,
					PodRegexp: regexp.MustCompile(
						fmt.Sprintf(
							"^%s$",
							regexp.QuoteMeta(pod.Name),
						),
					),
					ReplicasStatus: proto.ReplicasStatus{
						Desired:   newInt32Pointer(1),
						Current:   newInt32Pointer(1),
						Ready:     newInt32Pointer(1),
						Available: newInt32Pointer(1),
					},
				})
			}
		}

		return nil
	})

	group.Go(func() error {
		deployments, err := kube.GetDeployments()
		if err != nil {
			return karma.Format(
				err,
				"unable to get deployments",
			)
		}

		if deployments != nil {
			m.Lock()
			defer m.Unlock()

			rawResources["deployments"] = deployments

			for _, deployment := range deployments.Items {
				resources = append(resources, Resource{
					Kind:        "Deployment",
					Annotations: deployment.Annotations,
					Namespace:   deployment.Namespace,
					Name:        deployment.Name,
					Containers:  deployment.Spec.Template.Spec.Containers,
					PodRegexp: regexp.MustCompile(
						fmt.Sprintf(
							"^%s-[^-]+-[^-]+$",
							regexp.QuoteMeta(deployment.Name),
						),
					),
					ReplicasStatus: proto.ReplicasStatus{
						Desired:   deployment.Spec.Replicas,
						Current:   newInt32Pointer(deployment.Status.Replicas),
						Ready:     newInt32Pointer(deployment.Status.ReadyReplicas),
						Available: newInt32Pointer(deployment.Status.AvailableReplicas),
					},
				})
			}
		}

		return nil
	})

	group.Go(func() error {
		statefulSets, err := kube.GetStatefulSets()
		if err != nil {
			return karma.Format(
				err,
				"unable to get statefulSets",
			)
		}

		if statefulSets != nil {
			m.Lock()
			defer m.Unlock()

			rawResources["statefulSets"] = statefulSets

			for _, set := range statefulSets.Items {
				resources = append(resources, Resource{
					Kind:        "StatefulSet",
					Annotations: set.Annotations,
					Namespace:   set.Namespace,
					Name:        set.Name,
					Containers:  set.Spec.Template.Spec.Containers,
					PodRegexp: regexp.MustCompile(
						fmt.Sprintf(
							"^%s-([0-9]+)$",
							regexp.QuoteMeta(set.Name),
						),
					),
					ReplicasStatus: proto.ReplicasStatus{
						Desired:   set.Spec.Replicas,
						Current:   newInt32Pointer(set.Status.Replicas),
						Ready:     newInt32Pointer(set.Status.ReadyReplicas),
						Available: newInt32Pointer(set.Status.CurrentReplicas),
					},
				})
			}
		}

		return nil
	})

	group.Go(func() error {
		daemonSets, err := kube.GetDaemonSets()
		if err != nil {
			return karma.Format(
				err,
				"unable to get daemonSets",
			)
		}

		if daemonSets != nil {
			m.Lock()
			defer m.Unlock()

			rawResources["daemonSets"] = daemonSets

			for _, daemon := range daemonSets.Items {
				resources = append(resources, Resource{
					Kind:        "DaemonSet",
					Annotations: daemon.Annotations,
					Namespace:   daemon.Namespace,
					Name:        daemon.Name,
					Containers:  daemon.Spec.Template.Spec.Containers,
					PodRegexp: regexp.MustCompile(
						fmt.Sprintf(
							"^%s-[^-]+$",
							regexp.QuoteMeta(daemon.Name),
						),
					),
					ReplicasStatus: proto.ReplicasStatus{
						Desired:   newInt32Pointer(daemon.Status.DesiredNumberScheduled),
						Current:   newInt32Pointer(daemon.Status.CurrentNumberScheduled),
						Ready:     newInt32Pointer(daemon.Status.NumberReady),
						Available: newInt32Pointer(daemon.Status.NumberAvailable),
					},
				})
			}
		}

		return nil
	})

	group.Go(func() error {
		replicaSets, err := kube.GetReplicaSets()
		if err != nil {
			return karma.Format(
				err,
				"unable to get replicasets",
			)
		}

		if replicaSets != nil {
			m.Lock()
			defer m.Unlock()

			rawResources["replicaSets"] = replicaSets

			for _, replicaSet := range replicaSets.Items {
				// skipping when it is a part of another service
				if len(replicaSet.GetOwnerReferences()) > 0 {
					continue
				}
				resources = append(resources, Resource{
					Kind:        "ReplicaSet",
					Annotations: replicaSet.Annotations,
					Namespace:   replicaSet.Namespace,
					Name:        replicaSet.Name,
					Containers:  replicaSet.Spec.Template.Spec.Containers,
					PodRegexp: regexp.MustCompile(
						fmt.Sprintf(
							"^%s-[^-]+$",
							regexp.QuoteMeta(replicaSet.Name),
						),
					),
					ReplicasStatus: proto.ReplicasStatus{
						Desired:   replicaSet.Spec.Replicas,
						Current:   newInt32Pointer(replicaSet.Status.Replicas),
						Ready:     newInt32Pointer(replicaSet.Status.ReadyReplicas),
						Available: newInt32Pointer(replicaSet.Status.AvailableReplicas),
					},
				})
			}
		}

		return nil
	})

	group.Go(func() error {
		cronJobs, err := kube.GetCronJobs()
		if err != nil {
			return karma.Format(
				err,
				"unable to get cron jobs",
			)
		}

		if cronJobs != nil {
			m.Lock()
			defer m.Unlock()

			rawResources["cronJobs"] = cronJobs

			for _, cronJob := range cronJobs.Items {
				activeCount := int32(len(cronJob.Status.Active))
				resources = append(resources, Resource{
					Kind:        "CronJob",
					Annotations: cronJob.Annotations,
					Namespace:   cronJob.Namespace,
					Name:        cronJob.Name,
					Containers:  cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers,
					PodRegexp: regexp.MustCompile(
						fmt.Sprintf(
							"^%s-[^-]+-[^-]+$",
							regexp.QuoteMeta(cronJob.Name),
						),
					),
					ReplicasStatus: proto.ReplicasStatus{
						Current: newInt32Pointer(activeCount),
					},
				})
			}
		}

		return nil
	})

	group.Go(func() error {
		limitRangeList, err := kube.GetLimitRanges()
		if err != nil {
			return karma.Format(
				err,
				"unable to get limitRanges",
			)
		}

		if limitRangeList != nil {
			limitRanges = limitRangeList.Items

			m.Lock()
			defer m.Unlock()

			rawResources["limitRanges"] = limitRangeList
		}

		return nil
	})

	err = group.Wait()

	return
}

func newInt32Pointer(val int32) *int32 {
	res := new(int32)
	*res = val
	return res
}

// GetPods get kubernetes pods
func (kube *Kube) GetPods() (*kv1.PodList, error) {
	kube.logger.Debugf(nil, "{kubernetes} retrieving list of pods")
	podList, err := kube.core.Pods("").List(kmeta.ListOptions{})
	if err != nil {
		return nil, karma.Format(
			err,
			"unable to retrieve pods from all namespaces",
		)
	}

	return podList, nil
}

// GetReplicationControllers get replication controllers
func (kube *Kube) GetReplicationControllers() (
	*kv1.ReplicationControllerList, error,
) {
	kube.logger.Debugf(nil, "{kubernetes} retrieving list of replication controllers")
	controllers, err := kube.core.ReplicationControllers("").
		List(kmeta.ListOptions{})
	if err != nil {
		return nil, karma.Format(
			err,
			"unable to retrieve replication controllers from all namespaces",
		)
	}

	if controllers != nil {
		for _, item := range controllers.Items {
			maskPodSpec(&item.Spec.Template.Spec)
		}
	}

	return controllers, nil
}

// GetDeployments get deployments
func (kube *Kube) GetDeployments() (*kbeta2.DeploymentList, error) {
	kube.logger.Debugf(nil, "{kubernetes} retrieving list of deployments")
	deployments, err := kube.apps.Deployments("").List(kmeta.ListOptions{})
	if err != nil {
		return nil, karma.Format(
			err,
			"unable to retrieve deployments from all namespaces",
		)
	}

	if deployments != nil {
		for _, item := range deployments.Items {
			maskPodSpec(&item.Spec.Template.Spec)
		}
	}

	return deployments, nil
}

// GetStatefulSets get statuful sets
func (kube *Kube) GetStatefulSets() (
	*kbeta2.StatefulSetList, error,
) {
	kube.logger.Debugf(nil, "{kubernetes} retrieving list of stateful sets")
	statefulSets, err := kube.apps.
		StatefulSets("").
		List(kmeta.ListOptions{})
	if err != nil {
		return nil, karma.Format(
			err,
			"unable to retrieve stateful sets from all namespaces",
		)
	}

	if statefulSets != nil {
		for _, item := range statefulSets.Items {
			maskPodSpec(&item.Spec.Template.Spec)
		}
	}

	return statefulSets, nil
}

// GetDaemonSets get daemon sets
func (kube *Kube) GetDaemonSets() (
	*kbeta2.DaemonSetList, error,
) {
	kube.logger.Debugf(nil, "{kubernetes} retrieving list of daemon sets")
	daemonSets, err := kube.apps.
		DaemonSets("").
		List(kmeta.ListOptions{})
	if err != nil {
		return nil, karma.Format(
			err,
			"unable to retrieve daemon sets from all namespaces",
		)
	}

	if daemonSets != nil {
		for _, item := range daemonSets.Items {
			maskPodSpec(&item.Spec.Template.Spec)
		}
	}

	return daemonSets, nil
}

// GetReplicaSets get replicasets
func (kube *Kube) GetReplicaSets() (
	*kbeta2.ReplicaSetList, error,
) {
	kube.logger.Debugf(nil, "{kubernetes} retrieving list of replica sets")
	replicaSets, err := kube.apps.
		ReplicaSets("").
		List(kmeta.ListOptions{})
	if err != nil {
		return nil, karma.Format(
			err,
			"unable to retrieve replica sets from all namespaces",
		)
	}

	if replicaSets != nil {
		for _, item := range replicaSets.Items {
			maskPodSpec(&item.Spec.Template.Spec)
		}
	}

	return replicaSets, nil
}

// GetCronJobs get cron jobs
func (kube *Kube) GetCronJobs() (
	*kbeta1.CronJobList, error,
) {
	kube.logger.Debugf(nil, "{kubernetes} retrieving list of cron jobs")
	cronJobs, err := kube.batch.
		CronJobs("").
		List(kmeta.ListOptions{})
	if err != nil {
		return nil, karma.Format(
			err,
			"unable to retrieve cron jobs from all namespaces",
		)
	}

	if cronJobs != nil {
		for _, item := range cronJobs.Items {
			maskPodSpec(&item.Spec.JobTemplate.Spec.Template.Spec)
		}
	}

	return cronJobs, nil
}

// GetLimitRanges get limits and ranges for namespaces
func (kube *Kube) GetLimitRanges() (
	*kv1.LimitRangeList, error,
) {
	kube.logger.Debugf(nil, "{kubernetes} retrieving list of limitRanges from all namespaces")
	limitRanges, err := kube.core.LimitRanges("").
		List(kmeta.ListOptions{})
	if err != nil {
		return nil, karma.Format(
			err,
			"unable to retrieve list of limitRanges from all namespaces",
		)
	}

	return limitRanges, nil
}

func (kube *Kube) GetStatefulSet(namespace, name string) (
	*v1.StatefulSet, error,
) {
	statefulSet, err := kube.Clientset.AppsV1().
		StatefulSets(namespace).
		Get(name, kmeta.GetOptions{})
	if err != nil {
		return nil, karma.Format(
			err,
			"unable to retrieve statefulset %s/%s",
			namespace, name,
		)
	}

	if statefulSet != nil {
		maskPodSpec(&statefulSet.Spec.Template.Spec)
	}

	return statefulSet, nil
}

// SetResources set resources for a service
func (kube *Kube) SetResources(
	kind string,
	name string,
	namespace string,
	totalResources TotalResources,
) (skipped bool, err error) {
	if len(totalResources.Containers) == 0 && totalResources.Replicas == nil {
		return false, fmt.Errorf("invalid resources passed, nothing to change")
	}

	if strings.ToLower(kind) == "statefulset" {
		statefulSet, err := kube.GetStatefulSet(namespace, name)
		if err != nil {
			return false, karma.Format(err, "unable to get sts definition")
		}

		ctx := karma.
			Describe("replicas", statefulSet.Spec.Replicas)

		if statefulSet.Spec.Replicas != nil && *statefulSet.Spec.Replicas > 1 {
			msg := fmt.Sprintf("sts replicas %v > 1", statefulSet.Spec.Replicas)

			updateStrategy := statefulSet.Spec.UpdateStrategy.Type
			ctx = ctx.
				Describe("update-strategy", updateStrategy)

			if updateStrategy == v1.RollingUpdateStatefulSetStrategyType {

				// no rollingUpdate spec, then Partition = 0
				if statefulSet.Spec.UpdateStrategy.RollingUpdate != nil {
					partition := statefulSet.Spec.UpdateStrategy.RollingUpdate.Partition
					ctx = ctx.
						Describe("rolling-update-partition", partition)

					if partition != nil && *partition != 0 {
						return true, karma.Format(
							ctx.Reason(nil),
							msg+" and Spec.UpdateStrategy.RollingUpdate.Partition not equal 0",
						)
					}
				}

			} else {
				return true, karma.Format(
					ctx.Reason(nil),
					msg+" and Spec.UpdateStrategy not equal 'RollingUpdate'",
				)
			}
		}
	}

	var containerSpecs = make([]map[string]interface{}, len(totalResources.Containers))
	for i := range totalResources.Containers {

		container := totalResources.Containers[i]
		resources := map[string]map[string]interface{}{}

		if container.Limits.Memory != nil {
			memoryLimits := fmt.Sprintf("%dMi", *container.Limits.Memory)
			if _, ok := resources["limits"]; !ok {
				resources["limits"] = map[string]interface{}{}
			}
			resources["limits"]["memory"] = memoryLimits
		}
		if container.Limits.CPU != nil {
			cpuLimits := float64(*container.Limits.CPU) / milliCore
			if _, ok := resources["limits"]; !ok {
				resources["limits"] = map[string]interface{}{}
			}
			resources["limits"]["cpu"] = cpuLimits
		}

		if container.Requests.Memory != nil {
			memoryRequests := fmt.Sprintf("%dMi", *container.Requests.Memory)
			if _, ok := resources["requests"]; !ok {
				resources["requests"] = map[string]interface{}{}
			}
			resources["requests"]["memory"] = memoryRequests
		}
		if container.Requests.CPU != nil {
			cpuRequests := float64(*container.Requests.CPU) / milliCore
			if _, ok := resources["requests"]; !ok {
				resources["requests"] = map[string]interface{}{}
			}
			resources["requests"]["cpu"] = cpuRequests
		}

		if len(resources) == 0 {
			return false, fmt.Errorf(
				"invalid resources for container: %s",
				container.Name,
			)
		}

		spec := map[string]interface{}{
			"name":      container.Name,
			"resources": resources,
		}
		containerSpecs[i] = spec
	}

	body := map[string]interface{}{
		"kind": kind,
		"spec": map[string]interface{}{},
	}

	if len(containerSpecs) > 0 {
		spec := body["spec"].(map[string]interface{})
		spec["template"] = map[string]interface{}{
			"spec": map[string]interface{}{
				"containers": containerSpecs,
			},
		}
	}

	// setting replicas=nil makes k8s set the replicas to 1
	if totalResources.Replicas != nil && *totalResources.Replicas > 0 {
		spec := body["spec"].(map[string]interface{})
		spec["replicas"] = totalResources.Replicas
	}

	b, err := json.Marshal(body)
	if err != nil {
		return false, err
	}
	req := kube.ClientV1Beta2.RESTClient().Patch(types.StrategicMergePatchType).
		Resource(kind + "s").
		Namespace(namespace).
		Name(name).
		Body(bytes.NewBuffer(b))

	res := req.Do()

	_, err = res.Get()
	return false, err
}

func maskPodSpec(podSpec *kv1.PodSpec) {
	podSpec.Containers = maskContainers(podSpec.Containers)
	podSpec.InitContainers = maskContainers(podSpec.InitContainers)

}

func maskContainers(containers []kv1.Container) (
	masked []kv1.Container,
) {
	for _, container := range containers {
		container.Env = maskEnvVars(container.Env)
		container.Args = maskArgs(container.Args)
		masked = append(masked, container)
	}
	return
}

func maskEnvVars(env [] kv1.EnvVar) (masked [] kv1.EnvVar) {
	masked = make([]kv1.EnvVar, len(env))
	for i, envVar := range env {
		if envVar.Value != "" {
			envVar.Value = maskedValue
		}
		masked[i] = envVar
	}
	return
}

func maskArgs(args []string) (masked []string) {
	masked = make([]string, len(args))
	for i := range args {
		masked[i] = maskedValue
	}
	return
}
