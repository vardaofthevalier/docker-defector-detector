package main 

import (
	"os"
	"fmt"
	"log"
	"net"
	"flag"
	"time"
	"bytes"
	"regexp"
	"context"
	"strconv"
	"strings"
	"encoding/json"
	docker "github.com/docker/docker/client"
	types "github.com/docker/docker/api/types"
	swarm "github.com/docker/docker/api/types/swarm"
	filters "github.com/docker/docker/api/types/filters"
)

/* REQUIRED ENVIRONMENT VARIABLES
	- INTERVAL  		:   The time interval (in seconds) to start a new round of service checks (recommended: 1+ minutes)
	- ENVIRONMENT 		:	The lifecycle environment where all of the services are running
	- CONTAINER_NAME	:	The name of this container
	- SERVICE_NAME 		:	The name of the service running this container
	- DOCKER_HOST		:	tcp://<docker-host-ip>:<docker-host-port>
	- DOCKER_TSL_VERIFY	:	Should be set to false
*/

type SourceNode struct {
	Id string 	`json:"id"`
	Ip string 	`json:"ip"`
}

type TasksInfo struct {
	Num int 								`json:"total"`
	TaskToContainerIdMap map[string]string 	`json:"task_to_container_id_map"`
}

type ServiceCheckInfo struct {
	ServiceId string 					`json:"service_id"`
	ServiceName string 					`json:"service_name"`
	DesiredTasks TasksInfo				`json:"desired_tasks,omitempty"`
	StaleTasks TasksInfo				`json:"stale_tasks,omitempty"`
	ServiceIpMap map[string][]string 	`json:"service_ip_to_hosts_map,omitempty"`
	ServiceHostMap map[string][]string 	`json:"service_host_to_ip_map,omitempty"`
	Node SourceNode						`json:"source_node"`
	Environment string 					`json:"environment"`
	Messages []string 					`json:"messages"`
}

type ServiceCheckResponse struct {
	Status string  			`json:"status"`
	Source string 			`json:"source"`
	Info ServiceCheckInfo	`json:"info"`
}

type ApplicationErrorResponse struct {
	Status string 	`json:"status"`
	Source string 	`json:"source"`
	Info string 	`json:"info"`
}

func main() {
	var (
		logger = log.New(os.Stderr, "", log.LUTC)
		sleepInterval = os.Getenv("INTERVAL")
		containerName = os.Getenv("CONTAINER_NAME")
		serviceName = os.Getenv("SERVICE_NAME")
		environment = os.Getenv("ENVIRONMENT")
		nodeId string
		nodeIp string
		managerClient *docker.Client
		workerClient *docker.Client
		err error
		ctx = context.Background()
		output ApplicationErrorResponse
	)

	var debug = flag.Bool("debug", false, "If true, will output info messages in addition to error messages")
	flag.Parse()
	
	// create clients for connecting to the swarm manager daemon and also for the underlying host's daemon
	managerClient, err = docker.NewEnvClient()
	if err != nil {
		output.Status = "ERROR"
		output.Source = "environment"
		output.Info = fmt.Sprintf("There was a problem connecting to the Docker API server on the manager node: %v", err)
		LogResponse(output, logger, true)
	}

	os.Setenv("DOCKER_HOST", "")
	os.Setenv("DOCKER_TSL_VERIFY", "")
	workerClient, err = docker.NewEnvClient()
	if err != nil {
		output.Status = "ERROR"
		output.Source = "environment"
		output.Info = fmt.Sprintf("There was a problem connecting to the Docker API server on the underlying node: %v", err)
		LogResponse(output, logger, true)
	}

	// what node am I running on?
	containerNameFilter := filters.KeyValuePair{
		Key: "name",
		Value: containerName,
	}

	containerTaskFilter := filters.KeyValuePair{
		Key: "is-task",
		Value: "true",
	}

	containerFilters := types.ContainerListOptions{
		Filters: filters.NewArgs(containerNameFilter, containerTaskFilter),
	}

	var containers []types.Container
	containers, err = workerClient.ContainerList(ctx, containerFilters)
	if err != nil {
		output.Status = "ERROR"
		output.Source = "environment"
		output.Info = fmt.Sprintf("There was a problem getting the current container's info: %v", err)
		LogResponse(output, logger, true)
	}

	var tasks []swarm.Task
	taskFilter := filters.KeyValuePair{
		Key: "service",
		Value: serviceName,
	}

	taskFilters := types.TaskListOptions{
		Filters: filters.NewArgs(taskFilter),
	}

	tasks, err = managerClient.TaskList(ctx, taskFilters)
	if err != nil {
		output.Status = "ERROR"
		output.Source = "environment"
		output.Info = fmt.Sprintf("There was a problem getting the current container's task info: %v", err)
		LogResponse(output, logger, true)
	}

	for _, t := range tasks {
		if t.Status.ContainerStatus.ContainerID == containers[len(containers) - 1].ID {
			nodeId = t.NodeID
		}
	}

	if nodeId != "" {
		node, _, err := managerClient.NodeInspectWithRaw(ctx, nodeId)
		if err != nil {
			output.Status = "ERROR"
			output.Source = "environment"
			output.Info = fmt.Sprintf("There was a problem getting the current container's node info: %v", err)
			LogResponse(output, logger, true)
		} else {
			nodeIp = node.Status.Addr
		}
	} else {
		output.Status = "ERROR"
		output.Source = "environment"
		output.Info = fmt.Sprintf("There was a problem getting the current container's node info: %v", err)
		LogResponse(output, logger, true)
	}

	for {
		// get a list of all services
		var serviceList []swarm.Service
		serviceList, err = managerClient.ServiceList(ctx, types.ServiceListOptions{})
		if err != nil {
			output.Status = "ERROR"
			output.Source = "environment"
			output.Info = fmt.Sprintf("There was a problem getting a listing of services: %v", err)
			LogResponse(output, logger, true)
		}

		sleepInterval, err := strconv.Atoi(sleepInterval)
		if err != nil {
			output.Status = "ERROR"
			output.Source = "application"
			output.Info = fmt.Sprintf("There was a problem converting the interval string to an integer: %v", err)
			LogResponse(output, logger, true)
		}

		// for each service, run a goroutine to log any found service networking errors
		for _, svc := range serviceList {
			go CheckService(ctx, managerClient, svc, environment, nodeId, nodeIp, logger, *debug)
		}

		// sleep for interval
		time.Sleep(time.Duration(int64(sleepInterval)) * time.Second)
	}
}

func ConvertStructToJsonString(s interface{}) (string, error) {
	jsonString, err := json.Marshal(s)
	if err != nil {
		return "", err
	}

	jsonString = bytes.Replace(jsonString, []byte("\\u003e"), []byte(">"), -1)

	return fmt.Sprintf("%s", jsonString), nil
}

func LogResponse(o interface{}, logger *log.Logger, fatal bool) {
	jsonString, err := ConvertStructToJsonString(o) 
	if err != nil {
		logger.Fatalf("Couldn't convert struct to json: %v", err)
	} else {
		if fatal {
			logger.Fatal(jsonString)
		} else {
			logger.Print(jsonString)
		}
	}
}

func CheckService(ctx context.Context, client *docker.Client, svc swarm.Service, environment string, nodeId string, nodeIp string, logger *log.Logger, debug bool) {
	var err error
	var tasks []swarm.Task
	var output ServiceCheckResponse
	var info ServiceCheckInfo
	var node SourceNode
	var desiredTasksInfo TasksInfo 
	var staleTasksInfo TasksInfo
	output.Status = "INFO"
	output.Source = "service"
	node.Id = nodeId
	node.Ip = nodeIp
	info.Environment = environment
	info.ServiceName = svc.Spec.Name 
	info.ServiceId = svc.ID
	info.Node = node
	info.ServiceIpMap = map[string][]string{}
	info.ServiceHostMap = map[string][]string{}
	info.Messages = []string{}
	desiredTasksInfo.TaskToContainerIdMap = map[string]string{} 
	staleTasksInfo.TaskToContainerIdMap = map[string]string{}

	// get the tasks for the service
	filterArgs := filters.KeyValuePair{
		Key: "service",
		Value: svc.Spec.Name,
	}

	filters := types.TaskListOptions{
		Filters: filters.NewArgs(filterArgs),
	}

	tasks, err = client.TaskList(ctx, filters)
	if err != nil {
		info.Messages = append(info.Messages, fmt.Sprintf("Couldn't get a listing of tasks for service '%s': %v", svc.Spec.Name, err))
		output.Status = "ERROR"
		output.Source = "environment"
		output.Info = info
		LogResponse(output, logger, false)
		return
	}

	// get the number of replicas for the services
	var replicas int 
	var nodeList []swarm.Node
	if svc.Spec.Mode.Global != nil {
		nodeList, err = client.NodeList(ctx, types.NodeListOptions{})
		if err != nil {
			info.Messages = append(info.Messages, fmt.Sprintf("Couldn't get a listing of nodes in the swarm: %v", err))
			output.Status = "ERROR"
			output.Source = "environment"
			output.Info = info
			LogResponse(output, logger, false)
			return
		}
		replicas = len(nodeList)
	} else if svc.Spec.Mode.Replicated != nil { 
		replicas = int(*svc.Spec.Mode.Replicated.Replicas)
	} else {
		info.Messages = append(info.Messages, "Couldn't get replica information for service (Replicas == nil)")
		output.Status = "ERROR"
		output.Info = info
		LogResponse(output, logger, false)
		return
	}

	// get the container IDs for each of the replicas
	var goodContainerIds []string
	var goodTaskIds []string
	var badContainerIds []string
	var badTaskIds []string

	for _, t := range tasks {
		if t.DesiredState != "running" {
			if len(t.Status.ContainerStatus.ContainerID) >= 12 {
				badContainerIds = append(badContainerIds, t.Status.ContainerStatus.ContainerID[0:12])
			} else {
				badContainerIds = append(badContainerIds, t.Status.ContainerStatus.ContainerID)
			}
			staleTasksInfo.TaskToContainerIdMap[t.ID] = t.Status.ContainerStatus.ContainerID
			badTaskIds = append(badTaskIds, t.ID)
		} else {
			if len(t.Status.ContainerStatus.ContainerID) >= 12 {
				goodContainerIds = append(goodContainerIds, t.Status.ContainerStatus.ContainerID[0:12])
			} else {
				goodContainerIds = append(goodContainerIds, t.Status.ContainerStatus.ContainerID)
			}
			desiredTasksInfo.TaskToContainerIdMap[t.ID] = t.Status.ContainerStatus.ContainerID
			goodTaskIds = append(goodTaskIds, t.ID)
		}
	}

	desiredTasksInfo.Num = replicas 
	staleTasksInfo.Num = len(tasks) - replicas

	info.DesiredTasks = desiredTasksInfo
	info.StaleTasks = staleTasksInfo

	// lookup the IPs associated with the service hostname
	var vips []string
	hostname := fmt.Sprintf("tasks.%s", svc.Spec.Name)
	vips, err = net.LookupHost(hostname)
	if err != nil {
		info.Messages = append(info.Messages, fmt.Sprintf("Couldn't resolve service name '%s': %v", svc.Spec.Name, err))
		output.Status = "ERROR"
		output.Info = info
		LogResponse(output, logger, false)
		return
	} 

	info.ServiceHostMap[hostname] = vips

	// initial check: do the number of replicas equal the number of discovered ips?
	if len(vips) > replicas {
		output.Status = "ERROR"
		info.Messages = append(info.Messages, fmt.Sprintf("IP/Replica mismatch: number of ips=%d > replicas=%d", len(vips), replicas))
	}

	// check name resolution of IPs -- should resolve to something beginning with either the service name or one of the good container ids
	var hostnames []string
	goodContainerIdRegexp := regexp.MustCompile(fmt.Sprintf("^(%s).*$", strings.Join(goodContainerIds, "|")))
	badContainerIdRegexp := regexp.MustCompile(fmt.Sprintf("^(%s).*$", strings.Join(badContainerIds, "|")))
	badTaskIdRegexp := regexp.MustCompile(fmt.Sprintf("^%s\\.[0-9]*\\.(%s).*$", svc.Spec.Name, strings.Join(badTaskIds, "|")))
	goodTaskIdRegexp := regexp.MustCompile(fmt.Sprintf("^%s\\.[0-9]*\\.(%s).*$", svc.Spec.Name, strings.Join(goodTaskIds, "|")))
	svcNameRegexp := regexp.MustCompile(fmt.Sprintf("^%s\\.[0-9]*\\..*$", svc.Spec.Name))
	isSvcNameRegexp := regexp.MustCompile(fmt.Sprintf("^.*\\.[0-9]*\\..*$", svc.Spec.Name))
	for _, ip := range vips {
		hostnames, err = net.LookupAddr(ip)

		if err != nil {
			output.Status = "ERROR"
			info.Messages = append(info.Messages, fmt.Sprintf("Couldn't resolve ip '%s': %v", ip, err))
			continue
		}

		info.ServiceIpMap[ip] = hostnames
		
		for _, h := range hostnames {
			if svcNameRegexp.MatchString(h) {
				if !goodTaskIdRegexp.MatchString(h) || badTaskIdRegexp.MatchString(h) {
					output.Status = "ERROR"
					info.Messages = append(info.Messages, fmt.Sprintf("IP resolves to incorrect task: service '%s' resolves to host '%s'", svc.Spec.Name, h))
				} 
			} else {
				if isSvcNameRegexp.MatchString(h) {
					output.Status = "ERROR"
					info.Messages = append(info.Messages, fmt.Sprintf("IP resolves to incorrect service: service '%s' resolves to host '%s'", svc.Spec.Name, h))
				} else if !goodContainerIdRegexp.MatchString(h) || badContainerIdRegexp.MatchString(h) {
					output.Status = "ERROR"
					info.Messages = append(info.Messages, fmt.Sprintf("IP resolves to incorrect container: service '%s' resolves to host '%s'", svc.Spec.Name, h))
				} 
			}
		}
	}

	if len(info.Messages) == 0 {
		info.Messages = append(info.Messages, "OK")
	}
	output.Info = info

	if debug {
		LogResponse(output, logger, false)
	} else if output.Status == "ERROR" {
		LogResponse(output, logger, false)
	}
}