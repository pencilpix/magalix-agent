package metrics

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/MagalixCorp/magalix-agent/kuber"
	"github.com/MagalixCorp/magalix-agent/scanner"
	"github.com/MagalixTechnologies/alltogether-go"
	"github.com/MagalixTechnologies/log-go"
	"github.com/MagalixTechnologies/uuid-go"
	"github.com/reconquest/karma-go"
)

// used internally to set default values for some metrics.
// ex: throttling metrics, which won't exist in response if there is no throttle
// and we want default value of zero
type containerMetricStore struct {
	ApplicationID, ServiceID, ContainerID uuid.UUID
	Namespace, PodName, ContainerName     string
	Timestamp                             time.Time
	Value                                 float64
}

type KubeletSummaryContainer struct {
	Name      string
	StartTime time.Time

	CPU struct {
		Time                 time.Time
		UsageCoreNanoSeconds int64
	}

	Memory struct {
		Time     time.Time
		RSSBytes int64
	}

	RootFS struct {
		Time      time.Time
		UsedBytes int64
	}
}

// KubeletSummary a struct to hold kubelet summary
type KubeletSummary struct {
	Node struct {
		CPU struct {
			Time                 time.Time
			UsageCoreNanoSeconds int64
		}

		Memory struct {
			Time     time.Time
			RSSBytes int64
		}

		FS struct {
			Time          time.Time
			UsedBytes     int64
			CapacityBytes int64
		}

		Network struct {
			Time     time.Time
			RxBytes  int64
			RxErrors int64
			TxBytes  int64
			TxErrors int64
		}
	}
	Pods []struct {
		PodRef struct {
			Name      string
			Namespace string
		}

		Containers []KubeletSummaryContainer
		Network    struct {
			Time     time.Time
			RxBytes  int64
			RxErrors int64
			TxBytes  int64
			TxErrors int64
		}
	}
}

// KubeletValue timestamp value struct
type KubeletValue struct {
	Timestamp time.Time
	Value     int64
}

type backOff struct {
	sleep      time.Duration
	maxRetries int
}

type kubeletTimeouts struct {
	backoff backOff
}

// Kubelet kubelet client
type Kubelet struct {
	*log.Logger

	resolution    time.Duration
	previous      map[string]KubeletValue
	previousMutex *sync.Mutex
	timeouts      kubeletTimeouts
	kubeletClient *KubeletClient

	optInAnalysisData bool
}

// NewKubelet returns new kubelet
func NewKubelet(
	kubeletClient *KubeletClient,
	log *log.Logger,
	resolution time.Duration,
	timeouts kubeletTimeouts,
	optInAnalysisData bool,
) (*Kubelet, error) {
	kubelet := &Kubelet{
		Logger: log,

		kubeletClient: kubeletClient,

		resolution:    resolution,
		previous:      map[string]KubeletValue{},
		previousMutex: &sync.Mutex{},
		timeouts:      timeouts,

		optInAnalysisData: optInAnalysisData,
	}

	return kubelet, nil
}

// GetMetrics gets metrics
func (kubelet *Kubelet) GetMetrics(
	scanner *scanner.Scanner, tickTime time.Time,
) ([]*Metrics, map[string]interface{}, error) {
	kubelet.collectGarbage()

	metricsMutex := &sync.Mutex{}
	metrics := []*Metrics{}

	rawMutex := &sync.Mutex{}
	rawResponses := map[string]interface{}{}

	getKey := func(
		entity string,
		parentKey string,
		entityKey string,
		measurement string,
	) string {
		if parentKey != "" {
			parentKey = parentKey + ":"
		}
		return fmt.Sprintf(
			"%s-%s:%s%s",
			entity,
			measurement,
			parentKey,
			entityKey,
		)
	}

	calcRate := func(
		key string,
		timestamp time.Time,
		value int64,
		multiplier int64,
	) (int64, error) {

		previous, err := kubelet.getPreviousValue(key)

		if err != nil {
			return 0, err
		}

		duration := timestamp.UnixNano() - previous.Timestamp.UnixNano()

		if duration <= time.Second.Nanoseconds() {
			return 0, karma.Format(nil, "timestamp less than or equal previous one")
		}

		previousValue := previous.Value
		if previousValue > value {
			// we have a restart for this entity so the cumulative
			// value is reset so we should reset as well
			previousValue = 0
		}
		rate := multiplier * (value - previousValue) / duration

		return rate, nil
	}

	addMetricValue := func(
		measurementType string,
		measurement string,
		nodeID uuid.UUID,
		applicationID uuid.UUID,
		serviceID uuid.UUID,
		containerID uuid.UUID,
		podName string,
		timestamp time.Time,
		value int64,
	) {
		metricsMutex.Lock()
		defer metricsMutex.Unlock()
		if timestamp.Equal(time.Time{}) {
			kubelet.Errorf(
				karma.Describe("metric", measurement).
					Describe("type", measurementType).
					Describe("timestamp", timestamp).
					Reason(fmt.Errorf("invalid timestamp")),
				"invalid timestamp detect. defaulting to tickTime",
			)
			timestamp = tickTime
		}

		metrics = append(metrics, &Metrics{
			Name:        measurement,
			Type:        measurementType,
			Node:        nodeID,
			Application: applicationID,
			Service:     serviceID,
			Container:   containerID,
			Timestamp:   timestamp,
			Value:       value,
			PodName:     podName,
		})
	}
	addMetricValueWithTags := func(
		measurementType string,
		measurement string,
		nodeID uuid.UUID,
		applicationID uuid.UUID,
		serviceID uuid.UUID,
		containerID uuid.UUID,
		podName string,
		timestamp time.Time,
		value int64,
		additionalTags map[string]interface{},
	) {
		metricsMutex.Lock()
		defer metricsMutex.Unlock()
		metrics = append(metrics, &Metrics{
			Name:        measurement,
			Type:        measurementType,
			Node:        nodeID,
			Application: applicationID,
			Service:     serviceID,
			Container:   containerID,
			Timestamp:   timestamp,
			Value:       value,
			PodName:     podName,

			AdditionalTags: additionalTags,
		})
	}

	addMetricValueRate := func(
		measurementType string,
		parentKey string,
		entityKey string,
		measurement string,
		nodeID uuid.UUID,
		applicationID uuid.UUID,
		serviceID uuid.UUID,
		containerID uuid.UUID,
		pod string,
		timestamp time.Time,
		value int64,
		multiplier int64,
	) {
		if timestamp.Equal(time.Time{}) {
			kubelet.Errorf(
				karma.Describe("metric", measurement).
					Describe("type", measurementType).
					Describe("timestamp", timestamp).
					Reason(fmt.Errorf("invalid timestamp")),
				"{rate} invalid timestamp detect. defaulting to tickTime",
			)
			timestamp = tickTime
		}

		key := getKey(measurementType, parentKey, entityKey, measurement)
		rate, err := calcRate(key, timestamp, value, multiplier)
		kubelet.updatePreviousValue(key, &KubeletValue{
			Timestamp: timestamp,
			Value:     value,
		})

		if err != nil {
			kubelet.Warningf(
				karma.Describe("metric", measurement).
					Describe("type", measurementType).
					Describe("timestamp", timestamp).
					Reason(err),
				"can't calculate rate",
			)
			return
		}
		addMetricValue(
			measurementType,
			measurement,
			nodeID,
			applicationID,
			serviceID,
			containerID,
			pod,
			timestamp,
			rate,
		)

	}

	addRawResponse := func(nodeID uuid.UUID, data interface{}) {
		rawMutex.Lock()
		defer rawMutex.Unlock()
		rawResponses[nodeID.String()] = data
	}

	// scanner scans the nodes every 1m, so assume latest value is up to date
	nodes := scanner.GetNodes()
	nodesScanTime := scanner.NodesLastScanTime()

	addMetricValue(
		TypeCluster,
		"nodes/count",
		uuid.Nil,
		uuid.Nil,
		uuid.Nil,
		uuid.Nil,
		"",
		nodesScanTime,
		int64(len(nodes)),
	)

	instanceGroups := map[string]int64{}
	for _, node := range nodes {
		instanceGroup := ""
		if node.InstanceType != "" {
			instanceGroup = node.InstanceType
		}
		if node.InstanceSize != "" {
			instanceGroup += "." + node.InstanceSize
		}

		if _, ok := instanceGroups[instanceGroup]; !ok {
			instanceGroups[instanceGroup] = 0
		}

		instanceGroups[instanceGroup] = instanceGroups[instanceGroup] + 1
	}

	for instanceGroup, nodesCount := range instanceGroups {
		addMetricValueWithTags(
			TypeCluster,
			"nodes/count",
			uuid.Nil,
			uuid.Nil,
			uuid.Nil,
			uuid.Nil,
			"",
			nodesScanTime,
			nodesCount,
			map[string]interface{}{
				"instance_group": instanceGroup,
			},
		)
	}

	for _, node := range nodes {
		for _, measurement := range []struct {
			Name  string
			Time  time.Time
			Value int64
		}{
			{"cpu/node_capacity", nodesScanTime, int64(node.Capacity.CPU)},
			{"cpu/node_allocatable", nodesScanTime, int64(node.Allocatable.CPU)},
			{"memory/node_capacity", nodesScanTime, int64(node.Capacity.Memory)},
			{"memory/node_allocatable", nodesScanTime, int64(node.Allocatable.Memory)},
		} {
			addMetricValue(
				TypeNode,
				measurement.Name,
				node.ID,
				uuid.Nil,
				uuid.Nil,
				uuid.Nil,
				"",
				measurement.Time,
				measurement.Value,
			)
		}
	}

	pr, err := alltogether.NewConcurrentProcessor(
		nodes,
		func(node kuber.Node) error {
			kubelet.Infof(
				nil,
				"{kubelet} requesting metrics from node %s",
				node.Name,
			)

			var (
				cadvisorResponse []byte
				summaryBytes     []byte
				summary          KubeletSummary
			)
			err := kubelet.withBackoff(func() error {
				var err error
				summaryBytes, err = kubelet.kubeletClient.GetBytes(&node, "stats/summary")
				if err != nil {
					if strings.Contains(err.Error(), "the server could not find the requested resource") {
						kubelet.Warningf(err, "unable to get summary from node %q", node.Name)
						summaryBytes = []byte("{}")
						return nil
					}
					return karma.Format(
						err,
						"{kubelet} unable to get summary from node %q",
						node.Name,
					)
				}
				return nil
			})

			if err != nil {
				return err
			}

			if kubelet.optInAnalysisData {
				var summaryInterface interface{}
				err = json.Unmarshal(summaryBytes, &summaryInterface)
				if err != nil {
					kubelet.Errorf(
						err,
						"{kubelet} unable to unmarshal summary response to its raw interface",
					)
				}
				if summaryInterface != nil {
					addRawResponse(node.ID, &summaryInterface)
				}
			}

			err = json.Unmarshal(summaryBytes, &summary)
			if err != nil {
				return karma.Format(
					err,
					"{kubelet} unable to unmarshal summary response",
				)
			}

			for _, measurement := range []struct {
				Name  string
				Time  time.Time
				Value int64
			}{
				{"cpu/usage", summary.Node.CPU.Time, summary.Node.CPU.UsageCoreNanoSeconds},
				{"memory/rss", summary.Node.Memory.Time, summary.Node.Memory.RSSBytes},
				{"filesystem/usage", summary.Node.FS.Time, summary.Node.FS.UsedBytes},
				{"filesystem/node_capacity", summary.Node.FS.Time, summary.Node.FS.CapacityBytes},
				{"filesystem/node_allocatable", summary.Node.FS.Time, summary.Node.FS.CapacityBytes},
				{"network/tx", summary.Node.Network.Time, summary.Node.Network.TxBytes},
				{"network/rx", summary.Node.Network.Time, summary.Node.Network.RxBytes},
				{"network/tx_errors", summary.Node.Network.Time, summary.Node.Network.TxErrors},
				{"network/rx_errors", summary.Node.Network.Time, summary.Node.Network.RxErrors},
			} {
				addMetricValue(
					TypeNode,
					measurement.Name,
					node.ID,
					uuid.Nil,
					uuid.Nil,
					uuid.Nil,
					"",
					measurement.Time,
					measurement.Value,
				)
			}

			for _, measurement := range []struct {
				Name       string
				Time       time.Time
				Value      int64
				Multiplier int64
			}{
				{"cpu/usage_rate", summary.Node.CPU.Time, summary.Node.CPU.UsageCoreNanoSeconds, 1000},
				{"network/tx_rate", summary.Node.Network.Time, summary.Node.Network.TxBytes, 1e9},
				{"network/rx_rate", summary.Node.Network.Time, summary.Node.Network.RxBytes, 1e9},
				{"network/tx_errors_rate", summary.Node.Network.Time, summary.Node.Network.TxErrors, 1e9},
				{"network/rx_errors_rate", summary.Node.Network.Time, summary.Node.Network.RxErrors, 1e9},
			} {

				addMetricValueRate(
					TypeNode,
					"",
					node.ID.String(),
					measurement.Name,
					node.ID,
					uuid.Nil,
					uuid.Nil,
					uuid.Nil,
					"",
					measurement.Time,
					measurement.Value,
					measurement.Multiplier,
				)
			}

			throttleMetrics := map[uuid.UUID]map[string]*containerMetricStore{}

			for _, pod := range summary.Pods {
				applicationID, serviceID, ok := scanner.FindService(
					pod.PodRef.Namespace, pod.PodRef.Name,
				)

				if !ok {
					kubelet.Logger.Warningf(
						karma.Describe("namespace", pod.PodRef.Namespace).
							Describe("pod_name", pod.PodRef.Name).
							Reason("not found"),
						"can't find service for pod %s:%s",
						pod.PodRef.Namespace, pod.PodRef.Name,
					)
					continue
				}

				for _, measurement := range []struct {
					Name  string
					Time  time.Time
					Value int64
				}{
					{"network/tx", pod.Network.Time, pod.Network.TxBytes},
					{"network/rx", pod.Network.Time, pod.Network.TxBytes},
					{"network/tx_errors", pod.Network.Time, pod.Network.TxErrors},
					{"network/rx_errors", pod.Network.Time, pod.Network.RxErrors},
				} {
					addMetricValue(
						TypePod,
						measurement.Name,
						node.ID,
						applicationID,
						serviceID,
						uuid.Nil,
						pod.PodRef.Name,
						measurement.Time,
						measurement.Value,
					)
				}

				for _, measurement := range []struct {
					Name  string
					Time  time.Time
					Value int64
				}{
					{"network/tx_rate", pod.Network.Time, pod.Network.TxBytes},
					{"network/rx_rate", pod.Network.Time, pod.Network.TxBytes},
					{"network/tx_errors_rate", pod.Network.Time, pod.Network.TxErrors},
					{"network/rx_errors_rate", pod.Network.Time, pod.Network.RxErrors},
				} {
					addMetricValueRate(
						TypePod,
						pod.PodRef.Namespace,
						pod.PodRef.Name,
						measurement.Name,
						node.ID,
						applicationID,
						serviceID,
						uuid.Nil,
						pod.PodRef.Name,
						measurement.Time,
						measurement.Value,
						1e9,
					)
				}

				// NOTE: possible bug in cAdvisor
				// Sometimes, when a container is restarted cAdvisor don't
				// understand this. It don't delete old stats of the old deleted
				// container but creates new stats for the new one.
				// Hence, we get two stats for two containers with the same name
				// and this lead to expected behavior.
				// This workaround filter containers with the same name in the
				// the same pod and take only the newer started one.
				podContainers := map[string]KubeletSummaryContainer{}
				for _, container := range pod.Containers {
					if foundContainer, ok := podContainers[container.Name]; !ok {
						// add to unique containers
						podContainers[container.Name] = container
					} else {
						if container.StartTime.After(foundContainer.StartTime) {
							// override the old container with the new started
							// one
							podContainers[container.Name] = container
						}
					}
				}

				for _, container := range podContainers {
					applicationID, serviceID, identifiedContainer, ok := scanner.FindContainer(
						pod.PodRef.Namespace,
						pod.PodRef.Name,
						container.Name,
					)
					if !ok {
						kubelet.Logger.Warningf(
							karma.Describe("namespace", pod.PodRef.Namespace).
								Describe("pod_name", pod.PodRef.Name).
								Describe("container_name", container.Name).
								Reason("not found"),
							"can't find container for container %s:%s:%s",
							pod.PodRef.Namespace, pod.PodRef.Name, container.Name,
						)
						continue
					}

					for _, measurement := range []struct {
						Name  string
						Time  time.Time
						Value int64
					}{
						{"cpu/usage", container.CPU.Time, container.CPU.UsageCoreNanoSeconds},
						{"memory/rss", container.Memory.Time, container.Memory.RSSBytes},
						{"filesystem/usage", container.RootFS.Time, container.RootFS.UsedBytes},

						{"cpu/request", container.CPU.Time, identifiedContainer.Resources.SpecResourceRequirements.Requests.Cpu().MilliValue()},
						{"cpu/limit", container.CPU.Time, identifiedContainer.Resources.SpecResourceRequirements.Limits.Cpu().MilliValue()},

						{"memory/request", container.Memory.Time, identifiedContainer.Resources.SpecResourceRequirements.Requests.Memory().Value()},
						{"memory/limit", container.Memory.Time, identifiedContainer.Resources.SpecResourceRequirements.Limits.Memory().Value()},
					} {
						addMetricValue(
							TypePodContainer,
							measurement.Name,
							node.ID,
							applicationID,
							serviceID,
							identifiedContainer.ID,
							pod.PodRef.Name,
							measurement.Time,
							measurement.Value,
						)
					}

					addMetricValueRate(
						TypePodContainer,
						fmt.Sprintf("%s:%s", pod.PodRef.Namespace, pod.PodRef.Name),
						container.Name,
						"cpu/usage_rate",
						node.ID,
						applicationID,
						serviceID,
						identifiedContainer.ID,
						pod.PodRef.Name,
						container.CPU.Time,
						container.CPU.UsageCoreNanoSeconds,
						1000, // cpu_rate is in millicore
					)

					throttleMetrics[identifiedContainer.ID] = map[string]*containerMetricStore{}
					throttleMetrics[identifiedContainer.ID]["container_cpu_cfs/periods_total"] = defaultMetricStore(applicationID, serviceID, identifiedContainer, pod.PodRef.Namespace, pod.PodRef.Name, container)
					throttleMetrics[identifiedContainer.ID]["container_cpu_cfs_throttled/seconds_total"] = defaultMetricStore(applicationID, serviceID, identifiedContainer, pod.PodRef.Namespace, pod.PodRef.Name, container)
					throttleMetrics[identifiedContainer.ID]["container_cpu_cfs_throttled/periods_total"] = defaultMetricStore(applicationID, serviceID, identifiedContainer, pod.PodRef.Namespace, pod.PodRef.Name, container)
				}
			}

			err = kubelet.withBackoff(func() error {
				cadvisorResponse, err = kubelet.kubeletClient.GetBytes(
					&node,
					"metrics/cadvisor",
				)
				if err != nil {
					if strings.Contains(err.Error(), "the server could not find the requested resource") {
						kubelet.Warningf(err,
							"{cAdvisor} unable to get cAdvisor from node %q",
							node.Name,
						)
						cadvisorResponse = []byte{}
						return nil
					}
					return karma.Format(
						err,
						"{kubelet} unable to get cadvisor from node %q",
						node.Name,
					)
				}
				return nil
			})

			if err != nil {
				return err
			}

			cadvisor, err := decodeCAdvisorResponse(bytes.NewReader(cadvisorResponse))
			if err != nil {
				return karma.Format(err,
					"{kubelet} unable to read cadvisor response",
				)
			}

			now := time.Now().UTC()

			for _, metric := range []struct {
				Name string
				Ref  string
			}{
				{"container_cpu_cfs/periods_total", "container_cpu_cfs_periods_total"},
				{"container_cpu_cfs_throttled/periods_total", "container_cpu_cfs_throttled_periods_total"},
				{"container_cpu_cfs_throttled/seconds_total", "container_cpu_cfs_throttled_seconds_total"},
			} {
				for _, val := range cadvisor[metric.Ref] {
					podUID, containerName, _, value, ok := getCAdvisorContainerValue(val)
					if ok {
						_, _, containerID, _, ok := scanner.FindContainerByPodUIDContainerName(podUID, containerName)
						if ok {
							if storedMetrics, ok := throttleMetrics[containerID]; ok {
								if storedMetric, ok := storedMetrics[metric.Name]; ok {
									storedMetric.Value = value
								} else {
									kubelet.Error("no stored metric with name: %s", metric.Name)
								}
							} else {
								kubelet.Warning("found a container: %s in cAdvisor response that don't exist at summary response", containerName)
							}
						}
					}
				}
			}

			for _, storedMetrics := range throttleMetrics {
				for metricName, storedMetric := range storedMetrics {
					addMetricValue(
						TypePodContainer,
						metricName,
						node.ID,
						storedMetric.ApplicationID,
						storedMetric.ServiceID,
						storedMetric.ContainerID,
						storedMetric.PodName,
						summary.Node.CPU.Time,
						// TODO: send as float
						int64(storedMetric.Value),
					)

					rateValue := storedMetric.Value
					// TODO: cleanup when values are sent as floats
					// covert seconds to milliseconds
					if strings.Contains(metricName, "seconds") {
						rateValue *= 1000
					}

					addMetricValueRate(
						TypePodContainer,
						fmt.Sprintf("%s:%s", storedMetric.Namespace, storedMetric.PodName),
						storedMetric.ContainerName,
						metricName+"_rate",
						node.ID,
						storedMetric.ApplicationID,
						storedMetric.ServiceID,
						storedMetric.ContainerID,
						storedMetric.PodName,
						now,
						int64(rateValue),
						1e9,
					)
				}
			}

			return nil
		},
	)

	apps := scanner.GetApplications()
	scanTime := scanner.AppsLastScanTime()
	for _, app := range apps {
		for _, service := range app.Services {
			for _, container := range service.Containers {
				for _, measurement := range []struct {
					Name  string
					Value int64
				}{
					{"cpu/request", container.Resources.Requests.Cpu().MilliValue()},
					{"cpu/limit", container.Resources.Limits.Cpu().MilliValue()},

					{"memory/request", container.Resources.Requests.Memory().Value()},
					{"memory/limit", container.Resources.Limits.Memory().Value()},
				} {
					addMetricValue(
						TypePodContainer,
						measurement.Name,
						uuid.Nil,
						app.ID,
						service.ID,
						container.ID,
						"",
						scanTime,
						measurement.Value,
					)
				}

			}

		}
	}

	if err != nil {
		panic(err)
	}

	// Start concurrent getter of details:
	errs := pr.Do()
	if !errs.AllNil() {
		// Note: if one node fails we fail safe to allow other node metrics to flow.
		// Note: In cases where pods are replicated across nodes,
		// Note: it means that the metrics are misleading. However, It is the
		// Note: rule of resampler to validate the correctness of the metrics
		// Note: and drop bad points

		for _, err := range errs {
			if err != nil {
				kubelet.Errorf(
					karma.Format(err, "error while scraping node metrics"),
					"error while scraping nodes metrics",
				)
			}
		}
	}

	result := []*Metrics{}

	var context *karma.Context
	for _, metrics := range metrics {

		/*
			context = context.Describe(
				fmt.Sprintf(
					"%s %s %s %s",
					metrics.Node,
					metrics.Application,
					metrics.Service,
					metrics.Container,
				),
				metrics.Name,
			)
		*/

		result = append(result, metrics)
	}

	if len(metrics) > 0 {
		kubelet.Infof(
			context,
			"{kubelet} collected %d measurements with timestamp %s",
			len(metrics),
			metrics[0].Timestamp,
		)
	} else {
		kubelet.Infof(
			context,
			"{kubelet} collected %d measurements",
			len(metrics),
		)
	}

	if !kubelet.optInAnalysisData {
		rawResponses = nil
	}

	return result, rawResponses, nil
}

func defaultMetricStore(
	applicationID uuid.UUID, serviceID uuid.UUID,
	identifiedContainer *scanner.Container, namespace, podName string,
	container KubeletSummaryContainer,
) *containerMetricStore {
	return &containerMetricStore{
		ApplicationID: applicationID,
		ServiceID:     serviceID,
		ContainerID:   identifiedContainer.ID,
		Namespace:     namespace,
		PodName:       podName,
		ContainerName: container.Name,
		Timestamp:     container.CPU.Time,
		Value:         0,
	}
}

func (kubelet *Kubelet) collectGarbage() {
	for key, previous := range kubelet.previous {
		if time.Now().Sub(previous.Timestamp) > time.Hour {
			delete(kubelet.previous, key)
		}
	}
}

func (kubelet *Kubelet) getPreviousValue(key string) (*KubeletValue, error) {
	kubelet.previousMutex.Lock()
	defer kubelet.previousMutex.Unlock()

	previous, ok := kubelet.previous[key]

	if !ok {
		return nil, karma.Format(nil, "No previous value")
	}

	// make new copy
	return &KubeletValue{
		Value:     previous.Value,
		Timestamp: previous.Timestamp,
	}, nil
}
func (kubelet *Kubelet) updatePreviousValue(key string, value *KubeletValue) {
	kubelet.previousMutex.Lock()
	defer kubelet.previousMutex.Unlock()

	kubelet.previous[key] = *value
}

func (kubelet *Kubelet) withBackoff(fn func() error) error {
	maxRetry := kubelet.timeouts.backoff.maxRetries
	try := 0
	for {
		try++

		err := fn()
		if err == nil {
			return nil
		}

		if try > maxRetry {
			context := karma.
				Describe("retry", try).
				Describe("maxRetry", maxRetry).
				Reason(err)
			kubelet.Errorf(
				context,
				"unhandled error occurred, no more retrying",
			)

			return karma.Format(context, "max retries exceeded")
		}

		// NOTE max multiplier = 10
		// 300ms -> 600ms -> [...] -> 3000ms -> 300ms
		timeout := kubelet.timeouts.backoff.sleep * time.Duration((try-1)%10+1)

		kubelet.Warningf(
			karma.Describe("retry", try).Reason(err),
			"unhandled error occurred, retrying after %s",
			timeout,
		)

		time.Sleep(timeout)
	}
}
