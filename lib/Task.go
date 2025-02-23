package ecs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/cenkalti/backoff"
	humanize "github.com/dustin/go-humanize"
)

// Task represents a single, runnable task
type Task struct {
	Cluster           string
	Name              string
	Image             string
	ExecutionRoleArn  string
	RoleArn           string
	Family            string
	LogGroupName      string
	Detach            bool
	Public            bool
	Fargate           bool
	Deregister        bool
	Count             int64
	Memory            int64
	MemoryReservation int64
	CPUReservation    int64
	Publish           []string
	Environment       []string
	SecurityGroups    []string
	Subnets           []string
	Volumes           []string
	Command           []string
	TaskDefinition    ecs.TaskDefinition
	Tasks             []*ecs.Task
}

var (
	sess = session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
)

// Stop a task
func (t *Task) Stop() {
	var svc = ecs.New(sess)
	logInfo("Stopping tasks")
	for _, task := range t.Tasks {
		_, err := svc.StopTask(&ecs.StopTaskInput{
			Cluster: task.ClusterArn,
			Reason:  aws.String("recieved a ^C"),
			Task:    task.TaskArn,
		})

		if err != nil {
			logError(err)
		} else {
			logInfo("Successfully stopped " + *task.TaskArn)
		}
	}
}

// Run a task
func (t *Task) Run() error {
	var launchType string
	var publicIP string
	var svc = ecs.New(sess)
	t.createLogGroup()
	v, m := buildMountPoint(t.Volumes)

	if t.Family == "" {
		t.Family = t.Name
	}

	taskDefInput := ecs.RegisterTaskDefinitionInput{
		ContainerDefinitions: []*ecs.ContainerDefinition{
			&ecs.ContainerDefinition{
				Name:    aws.String(t.Family),
				Image:   aws.String(t.Image),
				Command: aws.StringSlice(t.Command),
				Cpu:     aws.Int64(t.CPUReservation),
				LogConfiguration: &ecs.LogConfiguration{
					LogDriver: aws.String("awslogs"),
					Options: aws.StringMap(map[string]string{
						"awslogs-group":         t.LogGroupName,
						"awslogs-region":        *sess.Config.Region,
						"awslogs-stream-prefix": t.Name,
					}),
				},
				Essential:    aws.Bool(true),
				Environment:  buildEnvironmentKeyValuePair(t.Environment),
				PortMappings: buildPortMapping(t.Publish),
				MountPoints:  m,
				VolumesFrom:  []*ecs.VolumeFrom{},
			},
		},
		Volumes:     v,
		Family:      aws.String(t.Name),
		TaskRoleArn: aws.String(t.RoleArn),
	}

	if t.Memory > 0 {
		taskDefInput.ContainerDefinitions[0].Memory = aws.Int64(t.Memory)
	}

	if t.MemoryReservation > 0 {
		taskDefInput.ContainerDefinitions[0].MemoryReservation = aws.Int64(t.MemoryReservation)
	}

	if t.Fargate {
		taskDefInput.RequiresCompatibilities = aws.StringSlice([]string{"FARGATE"})
		taskDefInput.NetworkMode = aws.String("awsvpc")
		taskDefInput.ExecutionRoleArn = aws.String(t.ExecutionRoleArn)
		taskDefInput.Cpu = aws.String(fmt.Sprintf("%d", t.CPUReservation))
		taskDefInput.Memory = aws.String(fmt.Sprintf("%d", t.MemoryReservation))
	}

	// Register a new task definition
	arn, err := t.upsertTaskDefinition(svc, &taskDefInput)
	if err != nil {
		fmt.Printf("Error creating task definition: %s", err.Error())
		os.Exit(1)
	}

	logInfo("Running task definition: " + *arn)

	// Build the task parametes
	runTaskInput := &ecs.RunTaskInput{
		Cluster:        aws.String(t.Cluster),
		Count:          aws.Int64(t.Count),
		StartedBy:      aws.String("ecs cli"),
		TaskDefinition: arn,
	}

	// Configure for Fargate
	if t.Fargate {

		if t.Public {
			publicIP = "ENABLED"
		} else {
			publicIP = "DISABLED"
		}
		launchType = "FARGATE"
		runTaskInput.NetworkConfiguration = &ecs.NetworkConfiguration{
			AwsvpcConfiguration: &ecs.AwsVpcConfiguration{
				AssignPublicIp: aws.String(publicIP),
				SecurityGroups: aws.StringSlice(t.SecurityGroups),
				Subnets:        aws.StringSlice(t.Subnets),
			},
		}

		// Default to EC2 launch tye
	} else {
		launchType = "EC2"
	}

	runTaskInput.LaunchType = aws.String(launchType)

	// Run the task
	runTaskResponse, err := svc.RunTask(runTaskInput)
	if err != nil {
		return err
	}

	for _, failure := range runTaskResponse.Failures {
		fmt.Printf("Unable to schedule task on: %s\n\t%s\n", *failure.Arn, *failure.Reason)
	}

	if len(runTaskResponse.Failures) > 0 && len(runTaskResponse.Tasks) == 0 {
		return errors.New("Unable to schedule task")
	}

	t.Tasks = runTaskResponse.Tasks
	return nil
}

// Stream logs to stdout
func (t *Task) Stream() {
	logInfo("Streaming from Cloudwatch Logs")
	var svc = cloudwatchlogs.New(sess)
	var re = regexp.MustCompile("[^/]*$")
	nextToken := ""
	for _, task := range t.Tasks {
		for {
			logEventsInput := cloudwatchlogs.GetLogEventsInput{
				StartFromHead: aws.Bool(true),
				LogGroupName:  aws.String(*t.TaskDefinition.ContainerDefinitions[0].LogConfiguration.Options["awslogs-group"]),
				LogStreamName: aws.String(t.Name + "/" + t.Name + "/" + re.FindString(*task.TaskArn)),
			}

			if nextToken != "" {
				logEventsInput.NextToken = aws.String(nextToken)
			}

			logEvents, err := svc.GetLogEvents(&logEventsInput)
			if err != nil {
				if awsErr, ok := err.(awserr.Error); ok {
					// Get error details
					if awsErr.Code() == "ResourceNotFoundException" {
						time.Sleep(time.Second * 5)
						continue
					}
				} else {
					logFatalError(err)
				}
			}

			for _, log := range logEvents.Events {
				logCloudWatchEvent(log)
			}

			if logEvents.NextForwardToken != nil {
				nextToken = *logEvents.NextForwardToken
			}

			time.Sleep(time.Second * 5)
		}
	}
}

// Check the container is still running
func (t *Task) Check() {
	var svc = ecs.New(sess)
	var cluster *string
	var stoppedCount int
	var exitCode int64 = 1
	var reportedPorts = false
	var ip *string
	var re = regexp.MustCompile("[^/]*$")
	for _, task := range t.Tasks {
		cluster = task.ClusterArn
		logInfo(fmt.Sprintf("https://console.aws.amazon.com/ecs/home?#/clusters/%s/tasks/%s/details", t.Cluster, re.FindString(*task.TaskArn)))
	}

	for {
		describeTasksInput := ecs.DescribeTasksInput{
			Cluster: cluster,
			Tasks:   t.taskIds(),
		}

		if len(describeTasksInput.Tasks) == 0 {
			fmt.Println("Task not yet registered")
			time.Sleep(time.Second * 5)
			continue
		}

		res, err := svc.DescribeTasks(&describeTasksInput)
		logError(err)

		for _, ecsTask := range res.Tasks {

			if ip == nil && ecsTask.ContainerInstanceArn != nil {
				res, err := svc.DescribeContainerInstances(&ecs.DescribeContainerInstancesInput{
					Cluster:            &t.Cluster,
					ContainerInstances: aws.StringSlice([]string{*ecsTask.ContainerInstanceArn}),
				})
				logError(err)
				// getEc2Ip
				ip = getEc2InstanceIp(*res.ContainerInstances[0].Ec2InstanceId)
				logInfo(fmt.Sprintf("Container is starting on EC2 instance %v (%v).", *res.ContainerInstances[0].Ec2InstanceId, *ip))
			}

			if !reportedPorts {
				for _, container := range ecsTask.Containers {

					if container.NetworkBindings != nil {
						for _, networkBind := range container.NetworkBindings {
							//  get container instance ip from container.ContainerInstanceArn
							logInfo(fmt.Sprintf("Container is available here\n\thttp://%v:%v\n\tTCP %v %v", *ip, *networkBind.HostPort, *ip, *networkBind.HostPort))
							reportedPorts = true
						}
					}
				}
			}

			if *ecsTask.LastStatus == "STOPPED" {
				for _, container := range ecsTask.Containers {
					if container.ExitCode != nil {
						exitCode = *container.ExitCode
					}

					logInfo(fmt.Sprintf("Task %v has stopped (exit code %v):\n\t%v", *ecsTask.TaskArn, exitCode, *ecsTask.StoppedReason))
					if container.Reason != nil {
						logInfo(fmt.Sprintf("\t%v", *container.Reason))
					}
					stoppedCount++
				}
			}
		}
		if stoppedCount == len(res.Tasks) && len(res.Tasks) != 0 {
			logInfo("All containers have exited")
			if !t.Deregister {
				t.deregister(svc)
			}
			time.Sleep(time.Second * 5) // give the logs another chance to come in
			os.Exit(int(exitCode))
		}
		if t.Detach {
			return
		}
		time.Sleep(time.Second * 5)

	}

}

func (t *Task) createLogGroup() {
	t.LogGroupName = "/" + t.Cluster + "/ecs/" + t.Name

	var svc = cloudwatchlogs.New(sess)
	var logGroupName = aws.String(t.LogGroupName)

	output, err := svc.DescribeLogGroups(&cloudwatchlogs.DescribeLogGroupsInput{
		LogGroupNamePrefix: logGroupName,
	})
	if err != nil {
		logError(err)
		return
	}
	if len(output.LogGroups) == 0 {
		logInfo(fmt.Sprintf("Creating Log Group %s\n", *logGroupName))
		svc.CreateLogGroup(&cloudwatchlogs.CreateLogGroupInput{
			LogGroupName: logGroupName,
		})
	}
}

func (t *Task) deregister(svc *ecs.ECS) {
	_, err := svc.DeregisterTaskDefinition(&ecs.DeregisterTaskDefinitionInput{
		TaskDefinition: t.TaskDefinition.TaskDefinitionArn,
	})
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func (t *Task) upsertTaskDefinition(svc *ecs.ECS, taskDefInput *ecs.RegisterTaskDefinitionInput) (*string, error) {
	var td ecs.TaskDefinition
	td.ContainerDefinitions = taskDefInput.ContainerDefinitions
	definitions, err := svc.ListTaskDefinitions(&ecs.ListTaskDefinitionsInput{
		FamilyPrefix: aws.String(t.Family),
		Sort:         aws.String("DESC"),
		MaxResults:   aws.Int64(10),
	})
	if err != nil {
		return nil, err
	}

	// Loop through previous task definitions to prevent duplicates
	for _, definition := range definitions.TaskDefinitionArns {
		d, err2 := svc.DescribeTaskDefinition(&ecs.DescribeTaskDefinitionInput{
			TaskDefinition: definition,
		})
		if err2 != nil {
			return nil, err2
		}
		old, err2 := json.Marshal(d.TaskDefinition.ContainerDefinitions)
		if err2 != nil {
			return nil, err2
		}
		new, err2 := json.Marshal(td.ContainerDefinitions)
		if err2 != nil {
			return nil, err2
		}

		if string(old) == string(new) {
			logInfo("Using previous task definition")
			t.TaskDefinition = *d.TaskDefinition
			return d.TaskDefinition.TaskDefinitionArn, nil
		}
	}

	// unable to find an old task definition, register a new one
	req, taskDef := svc.RegisterTaskDefinitionRequest(taskDefInput)

	// An operation that may fail.
	var retryCount int
	const maxRetries = 50
	backoffWithRetries := backoff.WithMaxRetries(backoff.NewExponentialBackOff(), maxRetries)
	cfg := req.Config.WithLogLevel(aws.LogDebugWithRequestRetries)
	operation := func() error {
		logInfo("Creating task definition")
		req.Retryable = aws.Bool(true)
		req.Config = *cfg
		err = req.Send()
		if err != nil {
			t := time.Now()
			t = t.Add(backoffWithRetries.NextBackOff())
			logInfo(fmt.Sprintf("error creating task definition (attempt %d of %d). Will retry %s: %s\n", retryCount, maxRetries, humanize.Time(t), err))
		}
		retryCount++
		return err
	}

	err = backoff.Retry(operation, backoffWithRetries)
	if err != nil {
		return nil, err
	}

	t.TaskDefinition = *taskDef.TaskDefinition
	return taskDef.TaskDefinition.TaskDefinitionArn, nil

}

func (t *Task) taskIds() (tasks []*string) {
	for _, task := range t.Tasks {
		tasks = append(tasks, task.TaskArn)
	}
	return tasks
}
