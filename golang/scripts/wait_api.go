// Waits for the Go EC2 (tag Name=dbos-client-go) to have its API up on :8000.
//
// Mirrors the Python "Wait for the EC2 API" CI step: Floci marks the instance
// "running" before the UserData (Go toolchain download + build) is done, so
// the API port is not ready until a while after `terraform apply`.
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func main() {
	endpoint := os.Getenv("FLOCI_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://localhost:4566"
	}

	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("floci", "floci", "")),
	)
	if err != nil {
		fmt.Println("aws config:", err)
		os.Exit(1)
	}
	c := ec2.NewFromConfig(cfg, func(o *ec2.Options) { o.BaseEndpoint = aws.String(endpoint) })

	deadline := time.Now().Add(300 * time.Second)
	for time.Now().Before(deadline) {
		out, err := c.DescribeInstances(context.Background(), &ec2.DescribeInstancesInput{})
		if err == nil {
			for _, r := range out.Reservations {
				for _, i := range r.Instances {
					if i.State == nil || i.State.Name != ec2types.InstanceStateNameRunning {
						continue
					}
					if instanceName(i.Tags) != "dbos-client-go" || i.PrivateIpAddress == nil {
						continue
					}
					conn, dialErr := net.DialTimeout("tcp", *i.PrivateIpAddress+":8000", 3*time.Second)
					if dialErr == nil {
						conn.Close()
						fmt.Println("Go EC2 API is up:", *i.PrivateIpAddress)
						return
					}
				}
			}
		}
		time.Sleep(5 * time.Second)
	}
	fmt.Println("Go EC2 API did not come up in 300s")
	os.Exit(1)
}

func instanceName(tags []ec2types.Tag) string {
	for _, tag := range tags {
		if tag.Key != nil && *tag.Key == "Name" && tag.Value != nil {
			return *tag.Value
		}
	}
	return ""
}
