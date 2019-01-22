package cmd

import (
	"log"
	"os"
	"os/signal"
	"sync"

	"github.com/justmiles/ecs-cli/lib"
	"github.com/spf13/cobra"
)

var (
	task ecs.Task
	wg   sync.WaitGroup
)

func init() {
	log.SetFlags(0)

	rootCmd.AddCommand(runCmd)
	runCmd.PersistentFlags().StringVarP(&task.Cluster, "cluster", "", "", "ECS cluster")
	runCmd.PersistentFlags().StringVarP(&task.Name, "name", "n", "ephemeral-task-from-ecs-cli", "Assign a name to the task")
	runCmd.PersistentFlags().StringVar(&task.Family, "family", "", "Family for ECS task")
	runCmd.PersistentFlags().StringVar(&task.ExecutionRoleArn, "execution-role", "", "Execution role ARN (required for Fargate)")
	runCmd.PersistentFlags().StringVar(&task.RoleArn, "role", "", "Task role ARN")
	runCmd.PersistentFlags().BoolVarP(&task.Detach, "detach", "d", false, "Run the task in the background")
	runCmd.PersistentFlags().Int64VarP(&task.Count, "count", "c", 1, "Spawn n tasks")
	runCmd.PersistentFlags().Int64VarP(&task.Memory, "memory", "m", 0, "Memory limit")
	runCmd.PersistentFlags().Int64Var(&task.CPUReservation, "cpu-reservation", 0, "CPU reservation")
	runCmd.PersistentFlags().Int64Var(&task.MemoryReservation, "memory-reservation", 2048, "Memory reservation")
	runCmd.PersistentFlags().StringArrayVarP(&task.Environment, "env", "e", nil, "Set environment variables")
	runCmd.PersistentFlags().StringArrayVarP(&task.Publish, "publish", "p", nil, "Publish a container's port(s) to the host")
	// TODO: attach a specific security group
	runCmd.PersistentFlags().StringArrayVar(&task.SecurityGroups, "security-groups", nil, "[TODO] Attach security groups to task")
	runCmd.PersistentFlags().StringArrayVar(&task.Subnets, "subnet", nil, "Subnet(s) where task should run")
	runCmd.PersistentFlags().StringArrayVarP(&task.Volumes, "volume", "v", nil, "Map volume to ECS Container Instance")
	// TODO: support assigning public ip address
	runCmd.PersistentFlags().BoolVar(&task.Public, "public", false, "assign public ip")
	runCmd.PersistentFlags().BoolVar(&task.Fargate, "fargate", false, "Launch in Fargate")
	runCmd.PersistentFlags().BoolVar(&task.Deregister, "no-dergister", false, "do not deregister the task definition")
	runCmd.Flags().SetInterspersed(false)
}

// process the list command
var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Run a command in a new task",
	Run: func(cmd *cobra.Command, args []string) {
		if len(args) < 1 {
			log.Fatal("Please pass an image to run")
		}

		task.Image = args[0]

		if len(args) > 1 {
			task.Command = args[1:len(args)]
		}

		// fargate validation
		if task.Fargate {
			if len(task.Subnets) == 0 {
				log.Fatal("Fargate requires at least one subnet (--subnet)")
			}
			if task.ExecutionRoleArn == "" {
				log.Fatal("Fargate requires an execution role (--execution-role)")
			}
		}
		// Run the task
		err := task.Run()
		check(err)

		if task.Detach {
			task.Check()
		} else {
			defer task.Stop()
			wg.Add(2)
			go task.Stream()
			go task.Check()

			if err != nil {
				log.Fatal(err.Error())
			}
			c := make(chan os.Signal, 1)
			signal.Notify(c, os.Interrupt)
			go func() {
				for sig := range c {
					log.Printf("I got a %T\n", sig)
					task.Stop()
					os.Exit(0)
				}
			}()

			wg.Wait()
		}

	},
}
