package http

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/skupperproject/skupper/api/types"
	"github.com/skupperproject/skupper/test/utils/base"
	"github.com/skupperproject/skupper/test/utils/constants"
	"github.com/skupperproject/skupper/test/utils/k8s"
	"gotest.tools/assert"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type HttpClusterTestRunner struct {
	base.ClusterTestRunnerBase
}

// test table
type test struct {
	name            string
	doc             string
	cluster         *base.ClusterContext
	numOfWorkers    string
	durationOfTests string
	jobName         string
	targetURL       string
}

func int32Ptr(i int32) *int32 { return &i }

var httpbinDep *appsv1.Deployment = &appsv1.Deployment{
	TypeMeta: metav1.TypeMeta{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
	},
	ObjectMeta: metav1.ObjectMeta{
		Name: "httpbin",
	},
	Spec: appsv1.DeploymentSpec{
		Replicas: int32Ptr(1),
		Selector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"application": "httpbin"},
		},
		Template: apiv1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"application": "httpbin",
				},
			},
			Spec: apiv1.PodSpec{
				Containers: []apiv1.Container{
					{
						Name:            "httpbin",
						Image:           "docker.io/kennethreitz/httpbin",
						ImagePullPolicy: apiv1.PullIfNotPresent,
						Ports: []apiv1.ContainerPort{
							{
								Name:          "http",
								Protocol:      apiv1.ProtocolTCP,
								ContainerPort: 8080,
							},
						},
						Command: []string{
							"gunicorn",
							"-b",
							"0.0.0.0:8080",
							"httpbin:app",
							"-k",
							"gevent",
						},
					},
				},
			},
		},
	},
}

var nghttp2Dep *appsv1.Deployment = &appsv1.Deployment{
	TypeMeta: metav1.TypeMeta{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
	},
	ObjectMeta: metav1.ObjectMeta{
		Name: "nghttp2",
	},
	Spec: appsv1.DeploymentSpec{
		Replicas: int32Ptr(1),
		Selector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"application": "nghttp2"},
		},
		Template: apiv1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"application": "nghttp2",
				},
			},
			Spec: apiv1.PodSpec{
				Containers: []apiv1.Container{
					{
						Name:            "nghttp2",
						Image:           "docker.io/svagi/nghttp2",
						ImagePullPolicy: apiv1.PullIfNotPresent,
						Ports: []apiv1.ContainerPort{
							{
								Name:          "nghttp2",
								Protocol:      apiv1.ProtocolTCP,
								ContainerPort: 8443,
							},
						},
						//docker run -p 8443:8443 --network my-bridge-network -it svagi/nghttp2 nghttpx  -f"0.0.0.0,8443;no-tls" -b172.18.0.2,80 -L INFO
						Command: []string{
							"nghttpx",
							"-f0.0.0.0,8443;no-tls",
							"-bhttpbin,8080",
							"-L",
							"INFO",
						},
					},
				},
			},
		},
	},
}

// HTTP2 Load Job
var h2loadJob = &batchv1.Job{
	ObjectMeta: metav1.ObjectMeta{
		Name: "h2load",
		//Namespace: namespace,
	},
	Spec: batchv1.JobSpec{
		BackoffLimit: int32Ptr(3),
		Template: apiv1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Name: "h2load",
			},
			Spec: apiv1.PodSpec{
				Containers: []apiv1.Container{
					{
						Name:  "h2load",
						Image: "docker.io/svagi/nghttp2",
						//Command: []string{"h2load", "-n10", "-c10", "-m10", "http://nghttp2:8443"},
						Command: []string{"h2load", "-n1000", "-c1", "-m1", "http://nghttp2:8443"},
						Env: []apiv1.EnvVar{
							{Name: "JOB", Value: "h2load"},
						},
						ImagePullPolicy: apiv1.PullAlways,
					},
				},
				RestartPolicy: apiv1.RestartPolicyNever,
			},
		},
	},
}

// Base HTTP1 concurrent requests with Hey
var h1HeyBaseJob = &batchv1.Job{
	ObjectMeta: metav1.ObjectMeta{
		Name: "h1heybasejob",
		//Namespace: namespace,
	},
	Spec: batchv1.JobSpec{
		BackoffLimit: int32Ptr(3),
		Template: apiv1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Name: "h1heybasejob",
			},
			Spec: apiv1.PodSpec{
				Containers: []apiv1.Container{
					{
						Name:            "h1heybase",
						Image:           "quay.io/skupper/hey",
						Command:         []string{"hey_linux_amd64"},
						ImagePullPolicy: apiv1.PullAlways,
					},
				},
				RestartPolicy: apiv1.RestartPolicyNever,
			},
		},
	},
}

func runHeyTesWithParameter(t *testing.T, cluster *base.ClusterContext, numOfWorkers string, durationOfTests string, jobName string, targetURL string) {

	waitJob := func(cc *base.ClusterContext, jobName string) {
		t.Helper()
		job, err := k8s.WaitForJob(cc.Namespace, cc.VanClient.KubeClient, jobName, constants.ImagePullingAndResourceCreationTimeout)
		assert.Assert(t, err)
		cc.KubectlExec("logs job/" + jobName)
		k8s.AssertJob(t, job)
	}

	jobsClient := cluster.VanClient.KubeClient.BatchV1().Jobs(cluster.Namespace)

	// Set the parameters for Hey
	h1HeyBaseJob.Spec.Template.Spec.Containers[0].Args = []string{"-c", numOfWorkers, "-z", durationOfTests, targetURL}

	// Set new JobName
	h1HeyBaseJob.ObjectMeta.Name = jobName
	h1HeyBaseJob.Spec.Template.Name = jobName
	h1HeyBaseJob.Spec.Template.Spec.Containers[0].Name = jobName

	_, err := jobsClient.Create(h1HeyBaseJob)
	assert.Assert(t, err)
	waitJob(cluster, jobName)

	_output, err := cluster.KubectlExec("logs job/" + jobName)
	assert.Assert(t, err)
	output := string(_output)

	// Check if tests passed
	retCode, errRegex := regexp.MatchString("\\[200\\].[[:digit:]]*.responses", output)
	assert.Assert(t, errRegex)
	assert.Assert(t, retCode)

	// Check if any tests did not pass
	retCode, errRegex = regexp.MatchString("\\[[3-5][0-9]+\\].[[:digit:]]*.responses", output)
	assert.Assert(t, errRegex)
	assert.Assert(t, !retCode)
}

// Create the test table for Hey and start tests
func runHeyTestTable(t *testing.T, jobCluster *base.ClusterContext) {

	testTable := []test{
		{
			name:            "h1hey5wrk30sec",
			doc:             "Send request using 5 concurrent workers during 30 seconds",
			cluster:         jobCluster,
			numOfWorkers:    "5",
			durationOfTests: "30s",
			jobName:         "h1hey5wrk30sec",
			targetURL:       "http://httpbin:8080",
		},
		{
			name:            "h1hey50wrk30sec",
			doc:             "Send request using 50 concurrent workers during 30 seconds",
			cluster:         jobCluster,
			numOfWorkers:    "50",
			durationOfTests: "30s",
			jobName:         "h1hey50wrk30sec",
			targetURL:       "http://httpbin:8080",
		},
		{
			name:            "h1hey5wrk60sec",
			doc:             "Send request using 5 concurrent workers during 60 seconds",
			cluster:         jobCluster,
			numOfWorkers:    "5",
			durationOfTests: "60s",
			jobName:         "h1hey5wrk60sec",
			targetURL:       "http://httpbin:8080",
		},
		{
			name:            "h1hey50wrk60sec",
			doc:             "Send request using 50 concurrent workers during 60 seconds",
			cluster:         jobCluster,
			numOfWorkers:    "50",
			durationOfTests: "60s",
			jobName:         "h1hey50wrk60sec",
			targetURL:       "http://httpbin:8080",
		},
	}

	// Iterate over test table
	for _, test := range testTable {
		t.Run(test.name, func(t *testing.T) {
			runHeyTesWithParameter(t, test.cluster, test.numOfWorkers, test.durationOfTests, test.jobName, test.targetURL)
		})
	}
}

func (r *HttpClusterTestRunner) RunTests(t *testing.T) {
	pubCluster1, err := r.GetPublicContext(1)
	assert.Assert(t, err)

	_, err = k8s.WaitForSkupperServiceToBeCreatedAndReadyToUse(pubCluster1.Namespace, pubCluster1.VanClient.KubeClient, "httpbin")
	assert.Assert(t, err)

	_, err = k8s.WaitForSkupperServiceToBeCreatedAndReadyToUse(pubCluster1.Namespace, pubCluster1.VanClient.KubeClient, "nghttp2")
	assert.Assert(t, err)

	runJob := func(cc *base.ClusterContext, jobName, testName string) {
		t.Helper()
		jobCmd := []string{"/app/http_test", "-test.run", testName}

		_, err = k8s.CreateTestJob(cc.Namespace, cc.VanClient.KubeClient, jobName, jobCmd)
		assert.Assert(t, err)
	}

	waitJob := func(cc *base.ClusterContext, jobName string) {
		t.Helper()
		job, err := k8s.WaitForJob(cc.Namespace, cc.VanClient.KubeClient, jobName, constants.ImagePullingAndResourceCreationTimeout)
		assert.Assert(t, err)
		cc.KubectlExec("logs job/" + jobName)
		k8s.AssertJob(t, job)
	}

	// Send GET requests via HTTPD1
	t.Run("http1", func(t *testing.T) {
		runJob(pubCluster1, "http1", "TestHttpJob")
		waitJob(pubCluster1, "http1")
	})

	// Send GET requests via HTTPD2
	t.Run("http2", func(t *testing.T) {
		runJob(pubCluster1, "http2", "TestHttp2Job")
		waitJob(pubCluster1, "http2")
	})

	// Send a huge load for HTTPD2
	t.Run("http2load", func(t *testing.T) {
		jobsClient := pubCluster1.VanClient.KubeClient.BatchV1().Jobs(pubCluster1.Namespace)
		_, err = jobsClient.Create(h2loadJob)
		assert.Assert(t, err)
		waitJob(pubCluster1, "h2load")

		_output, err := pubCluster1.KubectlExec("logs job/" + "h2load")
		assert.Assert(t, err)
		output := string(_output)
		assert.Assert(t, strings.Contains(output, "1000 succeeded"), output)
	})

	// Call the test table for Hey tests
	runHeyTestTable(t, pubCluster1)
}

func (r *HttpClusterTestRunner) Setup(ctx context.Context, t *testing.T) {
	prv1Cluster, err := r.GetPrivateContext(1)
	assert.Assert(t, err)

	err = base.SetupSimplePublicPrivateAndConnect(ctx, &r.ClusterTestRunnerBase, "http")
	assert.Assert(t, err)

	privateDeploymentsClient := prv1Cluster.VanClient.KubeClient.AppsV1().Deployments(prv1Cluster.Namespace)

	createDeploymentInPrivateSite := func(dep *appsv1.Deployment) {
		t.Helper()
		fmt.Println("Creating httpbin deployment...")
		result, err := privateDeploymentsClient.Create(dep)
		assert.Assert(t, err)

		fmt.Printf("Created deployment %q.\n", result.GetObjectMeta().GetName())
	}

	createDeploymentInPrivateSite(httpbinDep)
	createDeploymentInPrivateSite(nghttp2Dep)

	service := types.ServiceInterface{
		Address:  "httpbin",
		Protocol: "http",
		Port:     8080,
	}

	err = prv1Cluster.VanClient.ServiceInterfaceCreate(ctx, &service)
	assert.Assert(t, err)

	err = prv1Cluster.VanClient.ServiceInterfaceBind(ctx, &service, "deployment", "httpbin", "http", 0)
	assert.Assert(t, err)

	http2service := types.ServiceInterface{
		Address:  "nghttp2",
		Protocol: "http2",
		Port:     8443,
	}

	err = prv1Cluster.VanClient.ServiceInterfaceCreate(ctx, &http2service)
	assert.Assert(t, err)

	err = prv1Cluster.VanClient.ServiceInterfaceBind(ctx, &http2service, "deployment", "nghttp2", "http2", 0)
	assert.Assert(t, err)

	http21service := types.ServiceInterface{
		Address:  "nghttp1",
		Protocol: "http",
		Port:     8443,
	}

	err = prv1Cluster.VanClient.ServiceInterfaceCreate(ctx, &http21service)
	assert.Assert(t, err)

	err = prv1Cluster.VanClient.ServiceInterfaceBind(ctx, &http21service, "deployment", "nghttp2", "http", 0)
	assert.Assert(t, err)

}

func (r *HttpClusterTestRunner) Run(ctx context.Context, t *testing.T) {
	defer base.TearDownSimplePublicAndPrivate(&r.ClusterTestRunnerBase)
	r.Setup(ctx, t)
	r.RunTests(t)
}
