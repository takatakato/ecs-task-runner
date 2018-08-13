package etr

import (
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	cw "github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/docker/distribution/reference"
	"github.com/google/uuid"
	"github.com/pottava/ecs-task-runner/lib"
)

// Config is set of configurations
type Config struct {
	AwsAccessKey   *string
	AwsSecretKey   *string
	AwsRegion      *string
	EcsCluster     string
	Image          string
	Subnets        []*string
	SecurityGroups []*string
	CPU            *string
	Memory         *string
	NumberOfTasks  *int64
	TaskTimeout    *int64
}

// Run runs the docker image on Amazon ECS
func Run(conf Config) error {
	ctx := aws.BackgroundContext()
	if os.Getenv("APP_DEBUG") == "1" {
		lib.PrintJSON(conf)
	}
	// Check AWS credentials
	sess, err := lib.Session(conf.AwsAccessKey, conf.AwsSecretKey, conf.AwsRegion, nil)
	if err != nil {
		return err
	}
	account, err := sts.New(sess).GetCallerIdentityWithContext(ctx, nil)
	if err != nil {
		return errors.New("Provided AWS credentials are invalid")
	}
	if os.Getenv("APP_DEBUG") == "1" {
		lib.PrintJSON(account)
	}
	// Check existence of the image on ECR
	image, err := validateImageName(conf, sess, aws.StringValue(account.Account))
	if err != nil {
		return err
	}
	// Generate UUID
	id := fmt.Sprintf("ecs-task-runner-%s", uuid.New().String())

	// Create AWS resources
	taskARN, _, err := createResouces(ctx, sess, conf, id, image)
	if err != nil {
		deleteResouces(ctx, sess, id, taskARN)
		return err
	}
	// Run the ECS task
	tasks, err := run(ctx, sess, conf, taskARN, id)
	if err != nil {
		deleteResouces(ctx, sess, id, taskARN)
		return err
	}
	// Wait for its done
	if err = waitForTaskDone(ctx, sess, conf, tasks); err != nil {
		deleteResouces(ctx, sess, id, taskARN)
		return err
	}
	// Retrieve app log
	logs := retrieveLogs(ctx, sess, id, tasks)

	// Delete AWS resources
	deleteResouces(ctx, sess, id, taskARN)

	// Format the result
	result := map[string][]string{}
	seq := 1
	for _, value := range logs {
		messages := []string{}
		for _, event := range value {
			messages = append(messages, fmt.Sprintf(
				"%s: %s",
				time.Unix(aws.Int64Value(event.Timestamp)/1000, 0).Format(time.RFC3339),
				aws.StringValue(event.Message),
			))
		}
		result[fmt.Sprintf("container-%d", seq)] = messages
		seq++
	}
	lib.PrintJSON(result)
	return nil
}

func validateImageName(conf Config, sess *session.Session, account string) (*string, error) {
	imageHost, imageName, imageTag, err := parseImageName(conf.Image)
	if err != nil {
		log.New(os.Stderr, "", 0).Println("Provided image name is invalid.")
		return nil, err
	}
	// Try to make up ECR image name
	if !strings.Contains(aws.StringValue(imageHost), account) {
		imageName = aws.String(fmt.Sprintf(
			"%s/%s",
			aws.StringValue(imageHost),
			aws.StringValue(imageName),
		))
		imageHost = aws.String(fmt.Sprintf(
			"%s.dkr.ecr.%s.amazonaws.com",
			account,
			aws.StringValue(conf.AwsRegion),
		))
	}
	repos, err := ecr.New(sess).DescribeRepositories(&ecr.DescribeRepositoriesInput{
		RepositoryNames: []*string{imageName},
	})
	if err != nil {
		return nil, err
	}
	if len(repos.Repositories) == 0 {
		return nil, fmt.Errorf(
			"Specified image is not in your ECR repositories. [ Image: %s, AWS account: %s ]",
			aws.StringValue(imageName), account,
		)
	}
	return aws.String(fmt.Sprintf(
		"%s/%s:%s",
		aws.StringValue(imageHost),
		aws.StringValue(imageName),
		aws.StringValue(imageTag),
	)), nil
}

func parseImageName(value string) (*string, *string, *string, error) {
	ref, err := reference.Parse(value)
	if err != nil {
		return nil, nil, nil, err
	}
	imageHost := ""
	imageName := ""
	if candidate, ok := ref.(reference.Named); ok {
		imageHost, imageName = reference.SplitHostname(candidate)
	}
	imageTag := "latest"
	if candidate, ok := ref.(reference.Tagged); ok {
		imageTag = candidate.Tag()
	}
	return aws.String(imageHost), aws.String(imageName), aws.String(imageTag), nil
}

const (
	ecsExecutionPolicyArn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
	fargate               = "FARGATE"
	awsVPC                = "awsvpc"
	awsCWLogs             = "awslogs"
)

func createResouces(ctx aws.Context, sess *session.Session, conf Config, id string, image *string) (*string, *string, error) {
	// Make a temporary log group
	if err := createLogGroup(ctx, sess, conf, id); err != nil {
		log.New(os.Stderr, "", 0).Println("Error at create#createLogGroup")
		return nil, nil, err
	}
	// Make a temporary IAM role
	roleARN, err := createIAMRole(ctx, sess, conf, id)
	if err != nil {
		log.New(os.Stderr, "", 0).Println("Error at create#createIAMRole")
		return nil, nil, err
	}
	// Make a temporary task definition
	taskARN, err := registerTaskDef(ctx, sess, conf, id, image, aws.StringValue(roleARN))
	if err != nil {
		log.New(os.Stderr, "", 0).Println("Error at create#registerTaskDef")
		return nil, nil, err
	}
	return taskARN, roleARN, nil
}

func createLogGroup(ctx aws.Context, sess *session.Session, conf Config, id string) error {
	_, err := cw.New(sess).CreateLogGroupWithContext(ctx, &cw.CreateLogGroupInput{
		LogGroupName: aws.String(fmt.Sprintf("/ecs/%s", id)),
	})
	return err
}

func createIAMRole(ctx aws.Context, sess *session.Session, conf Config, id string) (*string, error) {
	out, err := iam.New(sess).CreateRoleWithContext(ctx, &iam.CreateRoleInput{
		RoleName: aws.String(id),
		AssumeRolePolicyDocument: aws.String(`{
  "Statement": [{
    "Effect": "Allow",
    "Action": "sts:AssumeRole",
    "Principal": {
      "Service": "ecs-tasks.amazonaws.com"
    }
  }]
}`),
		Path: aws.String("/"),
	})
	if err != nil {
		return nil, err
	}
	if _, err = iam.New(sess).AttachRolePolicyWithContext(ctx, &iam.AttachRolePolicyInput{
		RoleName:  aws.String(id),
		PolicyArn: aws.String(ecsExecutionPolicyArn),
	}); err != nil {
		return nil, err
	}
	if err = waitForPolicyActive(ctx, sess, id); err != nil {
		return nil, err
	}
	return out.Role.Arn, nil
}

func waitForPolicyActive(ctx aws.Context, sess *session.Session, id string) error {

	// Avoid the following error
	// ClientException: ECS was unable to assume the role that was provided for this task.
	time.Sleep(15 * time.Second)

	timeout := time.After(10 * time.Second)
	for {
		select {
		case <-timeout:
			return errors.New("The IAM role for the task did not get active")
		default:
			policies, err := iam.New(sess).ListAttachedRolePoliciesWithContext(ctx, &iam.ListAttachedRolePoliciesInput{
				RoleName: aws.String(id),
			})
			if err != nil {
				return err
			}
			if len(policies.AttachedPolicies) > 0 {
				return nil
			}
			time.Sleep(1 * time.Second)
		}
	}
}

func registerTaskDef(ctx aws.Context, sess *session.Session, conf Config, id string, image *string, role string) (*string, error) {
	input := ecs.RegisterTaskDefinitionInput{
		Family:                  aws.String(id),
		RequiresCompatibilities: []*string{aws.String(fargate)},
		ExecutionRoleArn:        aws.String(role),
		Cpu:                     conf.CPU,
		Memory:                  conf.Memory,
		NetworkMode:             aws.String(awsVPC),
		ContainerDefinitions: []*ecs.ContainerDefinition{
			&ecs.ContainerDefinition{
				Name:      aws.String("app"),
				Image:     image,
				Essential: aws.Bool(true),
				LogConfiguration: &ecs.LogConfiguration{
					LogDriver: aws.String(awsCWLogs),
					Options: map[string]*string{
						"awslogs-region":        conf.AwsRegion,
						"awslogs-group":         aws.String(fmt.Sprintf("/ecs/%s", id)),
						"awslogs-stream-prefix": aws.String("fargate"),
					},
				},
			},
		},
	}
	if os.Getenv("APP_DEBUG") == "1" {
		lib.PrintJSON(input)
	}
	out, err := ecs.New(sess).RegisterTaskDefinitionWithContext(ctx, &input)
	if err != nil {
		return nil, err
	}
	return out.TaskDefinition.TaskDefinitionArn, nil
}

func run(ctx aws.Context, sess *session.Session, conf Config, taskARN *string, id string) ([]*ecs.Task, error) {
	out, err := ecs.New(sess).RunTaskWithContext(ctx, &ecs.RunTaskInput{
		Cluster:        aws.String(conf.EcsCluster),
		LaunchType:     aws.String(fargate),
		TaskDefinition: taskARN,
		Count:          conf.NumberOfTasks,
		NetworkConfiguration: &ecs.NetworkConfiguration{
			AwsvpcConfiguration: &ecs.AwsVpcConfiguration{
				AssignPublicIp: aws.String("ENABLED"),
				Subnets:        conf.Subnets,
				SecurityGroups: conf.SecurityGroups,
			},
		},
	})
	if err != nil {
		return nil, err
	}
	return out.Tasks, nil
}

func waitForTaskDone(ctx aws.Context, sess *session.Session, conf Config, tasks []*ecs.Task) error {
	timeout := time.After(time.Duration(aws.Int64Value(conf.TaskTimeout)) * time.Minute)
	taskARNs := []*string{}
	for _, task := range tasks {
		taskARNs = append(taskARNs, task.TaskArn)
	}
	for {
		select {
		case <-timeout:
			return fmt.Errorf("The task did not finish in %d minutes", aws.Int64Value(conf.TaskTimeout))
		default:
			tasks, err := ecs.New(sess).DescribeTasksWithContext(ctx, &ecs.DescribeTasksInput{
				Cluster: aws.String(conf.EcsCluster),
				Tasks:   taskARNs,
			})
			if err != nil {
				return err
			}
			if len(tasks.Tasks) > 0 {
				done := true
				for _, task := range tasks.Tasks {
					done = done && strings.EqualFold(aws.StringValue(task.LastStatus), "STOPPED")
				}
				if done {
					if os.Getenv("APP_DEBUG") == "1" {
						lib.PrintJSON(tasks.Tasks)
					}
					return nil
				}
			}
			time.Sleep(5 * time.Second)
		}
	}
}

var regTaskID = regexp.MustCompile("task/(.*)")

func retrieveLogs(ctx aws.Context, sess *session.Session, id string, tasks []*ecs.Task) map[string][]*cw.OutputLogEvent {
	result := map[string][]*cw.OutputLogEvent{}

	for _, task := range tasks {
		taskID := ""
		matched := regTaskID.FindAllStringSubmatch(aws.StringValue(task.TaskArn), -1)
		if len(matched) > 0 && len(matched[0]) > 1 {
			taskID = matched[0][1]
		}
		out, err := cw.New(sess).GetLogEventsWithContext(ctx, &cw.GetLogEventsInput{
			LogGroupName:  aws.String(fmt.Sprintf("/ecs/%s", id)),
			LogStreamName: aws.String(fmt.Sprintf("fargate/app/%s", taskID)),
		})
		if err == nil {
			result[taskID] = out.Events
		}
	}
	return result
}

func deleteResouces(ctx aws.Context, sess *session.Session, id string, task *string) {

	// Delete the temporary task definition
	deregisterTaskDef(ctx, sess, task)

	// Delete the temporary IAM role
	deleteIAMRole(ctx, sess, id)

	// Delete the temporary log group
	deleteLogGroup(ctx, sess, id)
}

func deregisterTaskDef(ctx aws.Context, sess *session.Session, taskARN *string) error {
	_, err := ecs.New(sess).DeregisterTaskDefinitionWithContext(ctx, &ecs.DeregisterTaskDefinitionInput{
		TaskDefinition: taskARN,
	})
	return err
}

func deleteIAMRole(ctx aws.Context, sess *session.Session, id string) error {
	iam.New(sess).DetachRolePolicyWithContext(ctx, &iam.DetachRolePolicyInput{
		RoleName:  aws.String(id),
		PolicyArn: aws.String(ecsExecutionPolicyArn),
	})
	_, err := iam.New(sess).DeleteRoleWithContext(ctx, &iam.DeleteRoleInput{
		RoleName: aws.String(id),
	})
	return err
}

func deleteLogGroup(ctx aws.Context, sess *session.Session, id string) error {
	_, err := cw.New(sess).DeleteLogGroupWithContext(ctx, &cw.DeleteLogGroupInput{
		LogGroupName: aws.String(fmt.Sprintf("/ecs/%s", id)),
	})
	return err
}
