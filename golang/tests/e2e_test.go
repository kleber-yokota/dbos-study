// E2E: the API on EC2 (Go) dispatches a workflow and the worker (Go Lambda)
// processes it.
//
// Preconditions: Floci running and the Go stack applied (golang/terraform).
// The stack is independent of the Python stack (own RDS/Lambda/EC2).
package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/rds"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

const (
	flociEndpoint = "http://localhost:4566"
	apiPort       = 8000
	wfTimeout     = 120 * time.Second
)

func flociConfig(t *testing.T) aws.Config {
	t.Helper()
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("floci", "floci", "")),
	)
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}
	return cfg
}

func apiURL(t *testing.T) string {
	t.Helper()
	ec2c := ec2.NewFromConfig(flociConfig(t), func(o *ec2.Options) {
		o.BaseEndpoint = aws.String(flociEndpoint)
	})
	out, err := ec2c.DescribeInstances(context.Background(), &ec2.DescribeInstancesInput{})
	if err != nil {
		t.Fatalf("describe_instances: %v", err)
	}
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			if inst.State == nil || inst.State.Name != "running" {
				continue
			}
			name := ""
			for _, tag := range inst.Tags {
				if tag.Key != nil && *tag.Key == "Name" && tag.Value != nil {
					name = *tag.Value
				}
			}
			if name == "dbos-client-go" && inst.PrivateIpAddress != nil {
				return fmt.Sprintf("http://%s:%d", *inst.PrivateIpAddress, apiPort)
			}
		}
	}
	t.Fatal("EC2 instance dbos-client-go not found in running")
	return ""
}

func dbosClient(t *testing.T) dbos.Client {
	t.Helper()
	rdsc := rds.NewFromConfig(flociConfig(t), func(o *rds.Options) {
		o.BaseEndpoint = aws.String(flociEndpoint)
	})
	out, err := rdsc.DescribeDBInstances(context.Background(), &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String("dbos-go-db"),
	})
	if err != nil {
		t.Fatalf("describe_db_instances: %v", err)
	}
	if len(out.DBInstances) == 0 {
		t.Fatal("RDS dbos-go-db not found")
	}
	db := out.DBInstances[0]
	c, err := dbos.NewClient(context.Background(), dbos.ClientConfig{
		DatabaseURL: fmt.Sprintf("postgresql://dbos:dbos_local@%s:%d/dbos", *db.Endpoint.Address, int(*db.Endpoint.Port)),
		AppName:     "dbos-go-worker",
	})
	if err != nil {
		t.Fatalf("dbos client: %v", err)
	}
	return c
}

func dispatch(t *testing.T, base, name, value string) string {
	t.Helper()
	u := base + "/workflows?name=" + url.QueryEscape(name)
	if value != "" {
		u += "&value=" + url.QueryEscape(value)
	}
	resp, err := http.Post(u, "application/json", nil)
	if err != nil {
		t.Fatalf("dispatch %s: %v", name, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dispatch %s: %d %s", name, resp.StatusCode, body)
	}
	var out struct {
		WorkflowID string `json:"workflow_id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("parse response: %v (%s)", err, body)
	}
	return out.WorkflowID
}

func waitTerminal(c dbos.Client, id string) (dbos.WorkflowStatus, error) {
	deadline := time.Now().Add(wfTimeout)
	for {
		h, err := dbos.RetrieveWorkflow[string](c, id)
		if err == nil {
			st, err := h.GetStatus()
			if err == nil {
				switch st.Status {
				case dbos.WorkflowStatusSuccess,
					dbos.WorkflowStatusError,
					dbos.WorkflowStatusCancelled,
					dbos.WorkflowStatusMaxRecoveryAttemptsExceeded:
					return st, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return dbos.WorkflowStatus{}, fmt.Errorf("timeout waiting for %s", id)
		}
		time.Sleep(2 * time.Second)
	}
}

func TestDispatchProcessTask(t *testing.T) {
	base := apiURL(t)

	// Dispatch first: on a fresh stack the API's first dispatch wakes the
	// worker, which creates the schema and the queue (the API waits for
	// that before responding) — so the client below is safe.
	id := dispatch(t, base, "process_task", "")
	c := dbosClient(t)
	st, err := waitTerminal(c, id)
	if err != nil {
		t.Fatalf("waiting for workflow: %v", err)
	}
	if st.Status != dbos.WorkflowStatusSuccess {
		t.Fatalf("status=%s (expected SUCCESS)", st.Status)
	}
	h, err := dbos.RetrieveWorkflow[string](c, id)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	res, err := h.GetResult()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if !strings.HasSuffix(res, ":confirmed") {
		t.Fatalf("result=%q (expected suffix ':confirmed')", res)
	}
}

func postAction(t *testing.T, u string) {
	t.Helper()
	resp, err := http.Post(u, "application/json", nil)
	if err != nil {
		t.Fatalf("POST %s: %v", u, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s: %d %s", u, resp.StatusCode, body)
	}
}

// Cancel/resume of a non-existent workflow_id return 404, not 200.
func TestUnknownWorkflowEndpointsFail(t *testing.T) {
	base := apiURL(t)
	for _, suffix := range []string{"/cancel", "/resume"} {
		resp, err := http.Post(base+"/workflows/"+strings.Repeat("0", 32)+suffix, "application/json", nil)
		if err != nil {
			t.Fatalf("POST %s: %v", suffix, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s of a non-existent workflow: got %d (want 404)", suffix, resp.StatusCode)
		}
	}
}

// Killing an execution (cancel) and continuing the SAME workflow_id:
// no new workflow is created.
func TestCancelThenResumeSameWorkflowID(t *testing.T) {
	base := apiURL(t)

	id := dispatch(t, base, "test_workflow", "")
	c := dbosClient(t)

	// 2) kill the execution by workflow_id
	postAction(t, base+"/workflows/"+id+"/cancel")
	st, err := waitTerminal(c, id)
	if err != nil {
		t.Fatalf("waiting for workflow: %v", err)
	}
	if st.Status != dbos.WorkflowStatusCancelled {
		t.Fatalf("status=%s (expected CANCELLED)", st.Status)
	}

	// 3) resume the SAME workflow_id
	postAction(t, base+"/workflows/"+id+"/resume")
	st, err = waitTerminal(c, id)
	if err != nil {
		t.Fatalf("waiting for workflow: %v", err)
	}
	if st.Status != dbos.WorkflowStatusSuccess {
		t.Fatalf("status=%s (expected SUCCESS)", st.Status)
	}
	h, err := dbos.RetrieveWorkflow[string](c, id)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	res, err := h.GetResult()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if !strings.HasSuffix(res, ":confirmed") {
		t.Fatalf("result=%q (expected suffix ':confirmed')", res)
	}
}

// A planned failure does not take down the worker: the next dispatch completes.
// Step 2 is a NEW execution (new workflow_id), not a resume of the failed
// one — an ERROR workflow is terminal and is continued via its workflow_id
// (resume/fork); here we only verify that the worker keeps processing.
func TestWorkerSurvivesWorkflowError(t *testing.T) {
	base := apiURL(t)

	// 1) dispatch with value='exception' -> planned failure -> ERROR
	idFail := dispatch(t, base, "test_workflow", "exception")
	c := dbosClient(t)
	st, err := waitTerminal(c, idFail)
	if err != nil {
		t.Fatalf("waiting for workflow: %v", err)
	}
	if st.Status != dbos.WorkflowStatusError {
		t.Fatalf("status=%s (expected ERROR)", st.Status)
	}

	// 2) re-dispatch without the parameter -> the worker continues and completes
	idOK := dispatch(t, base, "test_workflow", "")
	if idOK == idFail {
		t.Fatal("re-dispatch should create a new workflow")
	}
	st, err = waitTerminal(c, idOK)
	if err != nil {
		t.Fatalf("waiting for workflow: %v", err)
	}
	if st.Status != dbos.WorkflowStatusSuccess {
		t.Fatalf("status=%s (expected SUCCESS)", st.Status)
	}
	h, err := dbos.RetrieveWorkflow[string](c, idOK)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	res, err := h.GetResult()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if !strings.HasSuffix(res, ":confirmed") {
		t.Fatalf("result=%q (expected suffix ':confirmed')", res)
	}
}
