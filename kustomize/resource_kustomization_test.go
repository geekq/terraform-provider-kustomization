package kustomize

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	k8smetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sunstructured "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	k8s "k8s.io/client-go/kubernetes"
	k8sclientcmd "k8s.io/client-go/tools/clientcmd"
)

// Basic test
func TestAccResourceKustomization_basic(t *testing.T) {

	resource.Test(t, resource.TestCase{
		//PreCheck:  func() { testAccPreCheck(t) },
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			//
			//
			// Applying initial config with a svc and deployment in a namespace
			{
				Config: testAccResourceKustomizationConfig_basicInitial("test_kustomizations/basic/initial"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("kustomization_resource.ns", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.svc", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.dep1", "id"),
				),
			},
			//
			//
			// Applying modified config adding another deployment to the namespace
			{
				Config: testAccResourceKustomizationConfig_basicModified("test_kustomizations/basic/modified"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("kustomization_resource.ns", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.svc", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.dep1", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.dep2", "id"),
				),
			},
			//
			//
			// Reverting back to initial config with only one deployment
			// check that second deployment was purged
			{
				Config: testAccResourceKustomizationConfig_basicInitial("test_kustomizations/basic/initial"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("kustomization_resource.ns", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.svc", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.dep1", "id"),
					testAccCheckDeploymentPurged("kustomization_resource.dep2"),
				),
			},
			//
			//
			// Test state import
			{
				ResourceName:      "kustomization_resource.ns",
				ImportStateId:     "_/Namespace/_/test-basic",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func testAccResourceKustomizationConfig_basicInitial(path string) string {
	return testAccDataSourceKustomizationConfig_basic(path) + `
resource "kustomization_resource" "ns" {
	manifest = data.kustomization_build.test.manifests["_/Namespace/_/test-basic"]
}

resource "kustomization_resource" "svc" {
	manifest = data.kustomization_build.test.manifests["_/Service/test-basic/test"]
}

resource "kustomization_resource" "dep1" {
	manifest = data.kustomization_build.test.manifests["apps/Deployment/test-basic/test"]
}
`
}

func testAccResourceKustomizationConfig_basicModified(path string) string {
	return testAccResourceKustomizationConfig_basicInitial(path) + `
resource "kustomization_resource" "dep2" {
	manifest = data.kustomization_build.test.manifests["apps/Deployment/test-basic/test2"]
}
`
}
func TestRecreateManuallyModifiedResources(t *testing.T) {

	resource.Test(t, resource.TestCase{
		//PreCheck:  func() { testAccPreCheck(t) },
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			//
			//
			// Applying initial config with a svc and deployment in a namespace
			{
				Config: testAccResourceKustomizationConfig_basicInitial("test_kustomizations/basic/initial"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("kustomization_resource.ns", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.dep1", "id"),
					delayForManualDebugging(10),
				),
			},
			//
			//
			// Simulate a manual change to the deployment bypassing terraform,
			// here - change the `scale` attribute of a Deployment directly via k8s API
			{
				Config: testAccResourceKustomizationConfig_basicInitial("test_kustomizations/basic/initial"),
				Check:  modifyDeploymentScale("test-basic", "test", 2),
			},
			//
			//
			// Reapply the same terraform definition again, expecting this to revert the manual change
			{
				Config: testAccResourceKustomizationConfig_basicInitial("test_kustomizations/basic/initial"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("kustomization_resource.ns", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.dep1", "id"),
					// TODO Also check the terraform (state) changes / plan
					delayForManualDebugging(10),
					validateDeploymentScale("test-basic", "test", 1),
				),
			},
			//
			//
			// Test state import
			{
				ResourceName:      "kustomization_resource.ns",
				ImportStateId:     "_/Namespace/_/test-basic",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

// Function to modify the scale of a Kubernetes Deployment directly via k8s API
func modifyDeploymentScale(namespace, name string, replicas int32) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		clientset, err := initializeKubernetesClient()
		if err != nil {
			return err
		}

		// Check the previous state of the Deployment
		deploymentClient := clientset.AppsV1().Deployments(namespace)
		deployment, err := deploymentClient.Get(context.TODO(), name, k8smetav1.GetOptions{})
		if err != nil {
			return err
		}

		// Scale Deployment to desired replicas
		deployment.Spec.Replicas = int32Ptr(replicas)
		_, err = deploymentClient.Update(context.TODO(), deployment, k8smetav1.UpdateOptions{})
		if err != nil {
			return err
		}

		// Add delay for changes to propagate
		// TODO Find some more robust approach than a delay
		time.Sleep(2 * time.Second)
		return nil
	}
}

// Function to validate the scale of a Kubernetes Deployment
func validateDeploymentScale(namespace, name string, expectedReplicas int32) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		clientset, err := initializeKubernetesClient()
		if err != nil {
			return err
		}

		// Get the Deployment
		deployment, err := clientset.AppsV1().Deployments(namespace).Get(context.TODO(), name, k8smetav1.GetOptions{})
		if err != nil {
			return err
		}

		// Validate the replica count
		if *deployment.Spec.Replicas != expectedReplicas {
			return fmt.Errorf("expected %d replicas, found %d", expectedReplicas, *deployment.Spec.Replicas)
		}
		return nil
	}
}

func initializeKubernetesClient() (*k8s.Clientset, error) {
	kubeconfig := os.Getenv("KUBECONFIG_PATH")
	if kubeconfig == "" {
		// TODO Show instructions to user?
	}
	config, err := k8sclientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}
	return k8s.NewForConfig(config)
}

// Helper function to create an int32 pointer required by k8s API
func int32Ptr(i int32) *int32 { return &i }

// Function to introduce a delay, helpful for observing k8s resources slowly changing in e.g. k9s
func delayForManualDebugging(seconds int) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		time.Sleep(time.Duration(seconds) * time.Second)
		return nil
	}
}

// Import test invalid id
func TestAccResourceKustomization_importInvalidID(t *testing.T) {

	resource.Test(t, resource.TestCase{
		//PreCheck:  func() { testAccPreCheck(t) },
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			{
				Config: testAccResourceKustomizationConfig_basicInitial("test_kustomizations/basic/initial"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("kustomization_resource.ns", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.svc", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.dep1", "id"),
				),
			},
			//
			//
			// Test state import
			{
				ResourceName:      "kustomization_resource.ns",
				ImportStateId:     "invalidID",
				ImportState:       true,
				ImportStateVerify: true,
				ExpectError:       regexp.MustCompile("invalid ID: \"invalidID\", valid IDs look like: \"_/Namespace/_/example\""),
			},
		},
	})
}

// Update_Inplace Test
func TestAccResourceKustomization_updateInplace(t *testing.T) {

	resource.Test(t, resource.TestCase{
		//PreCheck:  func() { testAccPreCheck(t) },
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			//
			//
			// Applying initial config with a svc and deployment in a namespace
			{
				Config: testAccResourceKustomizationConfig_updateInplace("test_kustomizations/update_inplace/initial"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("kustomization_resource.ns", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.svc", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.dep1", "id"),
				),
			},
			//
			//
			// Applying modified config adding an annotation to each resource
			{
				Config: testAccResourceKustomizationConfig_updateInplace("test_kustomizations/update_inplace/modified"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("kustomization_resource.ns", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.svc", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.dep1", "id"),
					testAccCheckManifestAnnotation("kustomization_resource.ns", "test_annotation", "added"),
					testAccCheckManifestAnnotation("kustomization_resource.svc", "test_annotation", "added"),
					testAccCheckManifestAnnotation("kustomization_resource.dep1", "test_annotation", "added"),
				),
			},
			//
			//
			// Applying initial config again, ensure annotations are removed again
			{
				Config: testAccResourceKustomizationConfig_updateInplace("test_kustomizations/update_inplace/initial"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("kustomization_resource.ns", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.svc", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.dep1", "id"),
					testAccCheckManifestAnnotationAbsent("kustomization_resource.ns", "test_annotation"),
					testAccCheckManifestAnnotationAbsent("kustomization_resource.svc", "test_annotation"),
					testAccCheckManifestAnnotationAbsent("kustomization_resource.dep1", "test_annotation"),
				),
			},
			//
			//
			// Test state import
			{
				ResourceName:      "kustomization_resource.ns",
				ImportStateId:     "_/Namespace/_/test-update-inplace",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func testAccResourceKustomizationConfig_updateInplace(path string) string {
	return testAccDataSourceKustomizationConfig_basic(path) + `
resource "kustomization_resource" "ns" {
	manifest = data.kustomization_build.test.manifests["_/Namespace/_/test-update-inplace"]
}

resource "kustomization_resource" "svc" {
	manifest = data.kustomization_build.test.manifests["_/Service/test-update-inplace/test"]
}

resource "kustomization_resource" "dep1" {
	manifest = data.kustomization_build.test.manifests["apps/Deployment/test-update-inplace/test"]
}
`
}

// Update_Recreate Test
func TestAccResourceKustomization_updateRecreate(t *testing.T) {

	resource.Test(t, resource.TestCase{
		//PreCheck:  func() { testAccPreCheck(t) },
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			//
			//
			// Applying initial config with a svc and deployment in a namespace
			{
				Config: testAccResourceKustomizationConfig_updateRecreate("test_kustomizations/update_recreate/initial"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("kustomization_resource.ns", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.svc", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.dep1", "id"),
				),
			},
			//
			//
			// Applying modified config changing the immutable label selectors
			{
				Config: testAccResourceKustomizationConfig_updateRecreate("test_kustomizations/update_recreate/modified"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("kustomization_resource.ns", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.svc", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.dep1", "id"),
					testAccCheckManifestSelector("kustomization_resource.dep1", "test-label", "added"),
				),
			},
			//
			//
			// Applying initial config again, ensure label selector is back to original state
			{
				Config: testAccResourceKustomizationConfig_updateRecreate("test_kustomizations/update_recreate/initial"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("kustomization_resource.ns", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.svc", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.dep1", "id"),
					testAccCheckManifestSelectorAbsent("kustomization_resource.dep1", "test-label"),
				),
			},
			//
			//
			// Test state import
			{
				ResourceName:      "kustomization_resource.ns",
				ImportStateId:     "_/Namespace/_/test-update-recreate",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func testAccResourceKustomizationConfig_updateRecreate(path string) string {
	return testAccDataSourceKustomizationConfig_basic(path) + `
resource "kustomization_resource" "ns" {
	manifest = data.kustomization_build.test.manifests["_/Namespace/_/test-update-recreate"]
}

resource "kustomization_resource" "svc" {
	manifest = data.kustomization_build.test.manifests["_/Service/test-update-recreate/test"]
}

resource "kustomization_resource" "dep1" {
	manifest = data.kustomization_build.test.manifests["apps/Deployment/test-update-recreate/test"]
}
`
}

// Update_Recreate_Name_Or_Namespace_Change Test
func TestAccResourceKustomization_updateRecreateNameOrNamespaceChange(t *testing.T) {

	resource.Test(t, resource.TestCase{
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			//
			//
			// Applying initial config with a svc and deployment in a namespace
			{
				Config: testAccResourceKustomizationConfig_updateRecreateNameOrNamespaceChange("test_kustomizations/update_recreate_name_or_namespace_change/initial"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("kustomization_resource.ns", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.cm", "id"),
				),
			},
			//
			//
			// Applying modified config changing the immutable label selectors
			{
				Config: testAccResourceKustomizationConfig_updateRecreateNameOrNamespaceChangeModified("test_kustomizations/update_recreate_name_or_namespace_change/modified"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("kustomization_resource.ns", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.cm", "id"),
				),
			},
		},
	})
}

func testAccResourceKustomizationConfig_updateRecreateNameOrNamespaceChange(path string) string {
	return testAccDataSourceKustomizationConfig_basic(path) + `
resource "kustomization_resource" "ns" {
	manifest = data.kustomization_build.test.manifests["_/Namespace/_/test-update-recreate-name-or-namespace-change"]
}

resource "kustomization_resource" "cm" {
	manifest = data.kustomization_build.test.manifests["_/ConfigMap/test-update-recreate-name-or-namespace-change/test"]
}
`
}

func testAccResourceKustomizationConfig_updateRecreateNameOrNamespaceChangeModified(path string) string {
	return testAccDataSourceKustomizationConfig_basic(path) + `
resource "kustomization_resource" "ns" {
	manifest = data.kustomization_build.test.manifests["_/Namespace/_/test-update-recreate-name-or-namespace-change-modified"]
}

resource "kustomization_resource" "cm" {
	manifest = data.kustomization_build.test.manifests["_/ConfigMap/test-update-recreate-name-or-namespace-change-modified/test"]
}
`
}

// Update_Recreate_StatefulSet Test
func TestAccResourceKustomization_updateRecreateStatefulSet(t *testing.T) {

	resource.Test(t, resource.TestCase{
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			//
			//
			// Applying initial statefulset
			{
				Config: testAccResourceKustomizationConfig_updateRecreateStatefulSet("test_kustomizations/update_recreate_statefulset/initial"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("kustomization_resource.ns", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.ss", "id"),
				),
			},
			//
			//
			// Applying modified statefulset that requires a destroy and recreate
			{
				Config: testAccResourceKustomizationConfig_updateRecreateStatefulSet("test_kustomizations/update_recreate_statefulset/modified"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("kustomization_resource.ns", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.ss", "id"),
				),
			},
		},
	})
}

func testAccResourceKustomizationConfig_updateRecreateStatefulSet(path string) string {
	return testAccDataSourceKustomizationConfig_basic(path) + `
resource "kustomization_resource" "ns" {
	manifest = data.kustomization_build.test.manifests["_/Namespace/_/test-update-recreate-statefulset"]
}

resource "kustomization_resource" "ss" {
	manifest = data.kustomization_build.test.manifests["apps/StatefulSet/test-update-recreate-statefulset/test"]
}
`
}

// Update_Recreate_RoleRef Test
func TestAccResourceKustomization_updateRecreateRoleRef(t *testing.T) {

	resource.Test(t, resource.TestCase{
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			//
			//
			// Applying initial role and rolebinding
			{
				Config: testAccResourceKustomizationConfig_updateRecreateRoleRefInitial("test_kustomizations/update_recreate_roleref/initial"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("kustomization_resource.ns", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.r", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.rb", "id"),
				),
			},
			//
			//
			// Applying changed roleRef
			{
				Config: testAccResourceKustomizationConfig_updateRecreateRoleRefModified("test_kustomizations/update_recreate_roleref/modified"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("kustomization_resource.ns", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.r", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.rb", "id"),
				),
			},
		},
	})
}

func testAccResourceKustomizationConfig_updateRecreateRoleRefInitial(path string) string {
	return testAccDataSourceKustomizationConfig_basic(path) + `
resource "kustomization_resource" "ns" {
	manifest = data.kustomization_build.test.manifests["_/Namespace/_/test-update-recreate-roleref"]
}

resource "kustomization_resource" "r" {
	manifest = data.kustomization_build.test.manifests["rbac.authorization.k8s.io/Role/test-update-recreate-roleref/test-initial"]
}

resource "kustomization_resource" "rb" {
	manifest = data.kustomization_build.test.manifests["rbac.authorization.k8s.io/RoleBinding/test-update-recreate-roleref/test"]
}
`
}

func testAccResourceKustomizationConfig_updateRecreateRoleRefModified(path string) string {
	return testAccDataSourceKustomizationConfig_basic(path) + `
resource "kustomization_resource" "ns" {
	manifest = data.kustomization_build.test.manifests["_/Namespace/_/test-update-recreate-roleref"]
}

resource "kustomization_resource" "r" {
	manifest = data.kustomization_build.test.manifests["rbac.authorization.k8s.io/Role/test-update-recreate-roleref/test-modified"]
}

resource "kustomization_resource" "rb" {
	manifest = data.kustomization_build.test.manifests["rbac.authorization.k8s.io/RoleBinding/test-update-recreate-roleref/test"]
}
`
}

// Update_Recreate_StorageClass Test
func TestAccResourceKustomization_updateRecreateStorageClass(t *testing.T) {

	resource.Test(t, resource.TestCase{
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			//
			//
			// Applying initial storage class
			{
				Config: testAccResourceKustomizationConfig_updateRecreateStorageClassProvisioner("test_kustomizations/update_recreate_storage_class/initial"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("kustomization_resource.scparam", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.scprov", "id"),
				),
			},
			//
			//
			// Applying changed storage class
			{
				Config: testAccResourceKustomizationConfig_updateRecreateStorageClassProvisioner("test_kustomizations/update_recreate_storage_class/modified"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("kustomization_resource.scparam", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.scprov", "id"),
				),
			},
		},
	})
}

func testAccResourceKustomizationConfig_updateRecreateStorageClassProvisioner(path string) string {
	return testAccDataSourceKustomizationConfig_basic(path) + `
resource "kustomization_resource" "scparam" {
	manifest = data.kustomization_build.test.manifests["storage.k8s.io/StorageClass/_/local-storage-parameters"]
}

resource "kustomization_resource" "scprov" {
	manifest = data.kustomization_build.test.manifests["storage.k8s.io/StorageClass/_/local-storage-provisioner"]
}

`
}

type readyCheckFunc func(u *k8sunstructured.Unstructured) (bool, error)

var waitSupportedResources = map[string]readyCheckFunc{
	"Deployment":  deploymentReady,
	"DaemonSet":   daemonsetReady,
	"StatefulSet": statefulSetReady,
}

func TestAccResourceKustomization_wait(t *testing.T) {
	for kind, readyCheck := range waitSupportedResources {
		now := time.Now()
		resource.Test(t, resource.TestCase{
			Providers: testAccProviders,
			Steps: []resource.TestStep{
				// Applying initial config with wait
				{
					Config: testAccResourceKustomizationConfig_wait("test_kustomizations/wait/initial", kind),
					Check: resource.ComposeAggregateTestCheckFunc(
						assertDurationIsShorterThan(now, 5*time.Minute),
						testAccCheckManifestNestedString("kustomization_resource.dep", "test", "spec", "selector", "matchLabels", "app"),
						testAccCheckResourceReady("kustomization_resource.dep", "test-wait", "test", kind, readyCheck),
					),
				},
				// Applying modified config updating the deployment annotation with wait
				{
					Config: testAccResourceKustomizationConfig_wait("test_kustomizations/wait/modified", kind),
					Check: resource.ComposeAggregateTestCheckFunc(
						assertDurationIsShorterThan(now, 1*time.Minute),
						testAccCheckManifestNestedString("kustomization_resource.dep", "this will cause a redeploy", "spec", "template", "metadata", "annotations", "new"),
						testAccCheckResourceReady("kustomization_resource.dep", "test-wait", "test", kind, readyCheck),
					),
				},
			},
		})
	}
}

func testAccResourceKustomizationConfig_wait(path string, kind string) string {
	return testAccDataSourceKustomizationConfig_basic(path) + fmt.Sprintf(`
resource "kustomization_resource" "ns" {
	manifest = data.kustomization_build.test.manifests["_/Namespace/_/test-wait"]
}
resource "kustomization_resource" "dep" {
	manifest = data.kustomization_build.test.manifests["apps/%s/test-wait/test"]
	wait     = true
	timeouts {
		create = "1m"
		update = "1m"
	}
}
`, kind)
}

func TestAccResourceKustomization_add_wait(t *testing.T) {
	for kind, readyCheck := range waitSupportedResources {
		now := time.Now()
		resource.Test(t, resource.TestCase{
			Providers: testAccProviders,
			Steps: []resource.TestStep{
				// Applying initial config with no wait
				{
					Config: testAccResourceKustomizationConfig_wait_off("test_kustomizations/wait-change/initial", kind),
					Check: resource.ComposeAggregateTestCheckFunc(
						assertDurationIsShorterThan(now, 5*time.Minute),
						testAccCheckManifestNestedString("kustomization_resource.dep", "test", "spec", "selector", "matchLabels", "app"),
					),
				},
				// Applying exactly the same configuration, but with wait turned on
				{
					Config: testAccResourceKustomizationConfig_wait_on("test_kustomizations/wait-change/initial", kind),
					Check: resource.ComposeAggregateTestCheckFunc(
						assertDurationIsShorterThan(now, 1*time.Minute),
						testAccCheckManifestNestedString("kustomization_resource.dep", "test", "spec", "selector", "matchLabels", "app"),
						testAccCheckResourceReady("kustomization_resource.dep", "test-wait-change", "test", kind, readyCheck),
					),
				},
			},
		})
	}
}

func testAccResourceKustomizationConfig_wait_off(path string, kind string) string {
	return testAccDataSourceKustomizationConfig_basic(path) + fmt.Sprintf(`
resource "kustomization_resource" "ns" {
	manifest = data.kustomization_build.test.manifests["_/Namespace/_/test-wait-change"]
}
resource "kustomization_resource" "dep" {
	manifest = data.kustomization_build.test.manifests["apps/%s/test-wait-change/test"]
}
`, kind)
}

func testAccResourceKustomizationConfig_wait_on(path string, kind string) string {
	return testAccDataSourceKustomizationConfig_basic(path) + fmt.Sprintf(`
resource "kustomization_resource" "ns" {
	manifest = data.kustomization_build.test.manifests["_/Namespace/_/test-wait-change"]
}
resource "kustomization_resource" "dep" {
	manifest = data.kustomization_build.test.manifests["apps/%s/test-wait-change/test"]
	wait = true
	timeouts {
		create = "1m"
		update = "1m"
	}
}
`, kind)
}

func TestAccResourceKustomization_wait_failure(t *testing.T) {
	for kind, readyCheck := range waitSupportedResources {
		now := time.Now()

		resource.Test(t, resource.TestCase{
			Providers: testAccProviders,
			Steps: []resource.TestStep{
				// Applying initial config with a failing deployment with wait
				{
					Config: testAccResourceKustomizationConfig_wait_failure("test_kustomizations/wait-fail/initial", kind),
					Check: resource.ComposeAggregateTestCheckFunc(
						testAccCheckResourceNotReady("kustomization_resource.dep", "test-wait-fail", "test", kind, readyCheck),
						assertDurationIsLongerThan(now, 1*time.Minute),
					),
					ExpectError: regexp.MustCompile(fmt.Sprintf("timed out creating/updating %s test-wait-fail/test:", kind)),
				},
			},
		})
	}
}

func testAccResourceKustomizationConfig_wait_failure(path string, kind string) string {
	return testAccDataSourceKustomizationConfig_basic(path) + fmt.Sprintf(`
resource "kustomization_resource" "ns" {
	manifest = data.kustomization_build.test.manifests["_/Namespace/_/test-wait-fail"]
}
resource "kustomization_resource" "dep" {
	manifest = data.kustomization_build.test.manifests["apps/%s/test-wait-fail/test"]
	wait     = true
	timeouts {
		create = "1m"
	}
}
`, kind)
}

func TestAccResourceKustomization_nowait(t *testing.T) {
	for kind, readyCheck := range waitSupportedResources {
		resource.Test(t, resource.TestCase{
			Providers: testAccProviders,
			Steps: []resource.TestStep{
				// Applying initial config without wait so shouldn't be ready immediately after creation
				{
					Config: testAccResourceKustomizationConfig_nowait("test_kustomizations/nowait/initial", kind),
					Check: resource.ComposeAggregateTestCheckFunc(
						testAccCheckManifestNestedString("kustomization_resource.dep", "test", "spec", "selector", "matchLabels", "app"),
						testAccCheckResourceNotReady("kustomization_resource.dep", "test-nowait", "test", kind, readyCheck),
					),
				},
				// Applying modified config updating the deployment and statefulset annotation without wait,
				// so we don't immediately expect the annotation to be present
				{
					Config: testAccResourceKustomizationConfig_nowait("test_kustomizations/nowait/modified", kind),
					Check: resource.ComposeAggregateTestCheckFunc(
						testAccCheckManifestNestedString("kustomization_resource.dep", "this will cause a redeploy", "spec", "template", "metadata", "annotations", "new"),
						testAccCheckResourceNotReady("kustomization_resource.dep", "test-nowait", "test", kind, readyCheck),
					),
				},
			},
		})
	}
}

func testAccResourceKustomizationConfig_nowait(path string, kind string) string {
	return testAccDataSourceKustomizationConfig_basic(path) + fmt.Sprintf(`
resource "kustomization_resource" "ns" {
	manifest = data.kustomization_build.test.manifests["_/Namespace/_/test-nowait"]
}

resource "kustomization_resource" "dep" {
	manifest = data.kustomization_build.test.manifests["apps/%s/test-nowait/test"]
}
`, kind)
}

// Upgrade_API_Version Test
func TestAccResourceKustomization_upgradeAPIVersion(t *testing.T) {

	resource.Test(t, resource.TestCase{
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			//
			//
			// Applying initial test.example.com/v1alpha1 custom objects
			{
				Config: testAccResourceKustomizationConfig_upgradeAPIVersion("test_kustomizations/upgrade_api_version/initial"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("kustomization_resource.ns", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.clusteredcrd", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.namespacedcrd", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.clusteredco", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.namespacedco", "id"),
					testAccCheckManifestNestedString("kustomization_resource.clusteredco", "test.example.com/v1alpha1", "apiVersion"),
					testAccCheckManifestNestedString("kustomization_resource.clusteredco", "test.example.com/v1alpha1", "apiVersion"),
				),
			},
			//
			//
			// Update custom objects to v1beta1
			{
				Config: testAccResourceKustomizationConfig_upgradeAPIVersion("test_kustomizations/upgrade_api_version/modified"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("kustomization_resource.ns", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.clusteredcrd", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.namespacedcrd", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.clusteredco", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.namespacedco", "id"),
					testAccCheckManifestNestedString("kustomization_resource.clusteredco", "test.example.com/v1beta1", "apiVersion"),
					testAccCheckManifestNestedString("kustomization_resource.clusteredco", "test.example.com/v1beta1", "apiVersion"),
				),
			},
		},
	})
}

func testAccResourceKustomizationConfig_upgradeAPIVersion(path string) string {
	return testAccDataSourceKustomizationConfig_basic(path) + `
resource "kustomization_resource" "ns" {
	manifest = data.kustomization_build.test.manifests["_/Namespace/_/test-upgrade-api-version"]
}

resource "kustomization_resource" "clusteredcrd" {
	manifest = data.kustomization_build.test.manifests["apiextensions.k8s.io/CustomResourceDefinition/_/clusteredcrds.test.example.com"]
}

resource "kustomization_resource" "namespacedcrd" {
	manifest = data.kustomization_build.test.manifests["apiextensions.k8s.io/CustomResourceDefinition/_/namespacedcrds.test.example.com"]
}

resource "kustomization_resource" "clusteredco" {
	manifest = data.kustomization_build.test.manifests["test.example.com/Clusteredcrd/_/clusteredco"]
}

resource "kustomization_resource" "namespacedco" {
	manifest = data.kustomization_build.test.manifests["test.example.com/Namespacedcrd/test-upgrade-api-version/namespacedco"]
}
`
}

// CRD Test
func TestAccResourceKustomization_crd(t *testing.T) {

	resource.Test(t, resource.TestCase{
		//PreCheck:  func() { testAccPreCheck(t) },
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			//
			//
			// Applying both namespaced and cluster wide CRD
			// and one custom object of each CRD
			{
				Config: testAccResourceKustomizationConfig_crd("test_kustomizations/crd/initial"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(
						"kustomization_resource.clusteredcrd",
						"id"),
					resource.TestCheckResourceAttrSet(
						"kustomization_resource.namespacedcrd",
						"id"),
					resource.TestCheckResourceAttrSet(
						"kustomization_resource.clusteredco",
						"id"),
					resource.TestCheckResourceAttrSet(
						"kustomization_resource.namespacedco",
						"id"),
					resource.TestCheckResourceAttrSet(
						"kustomization_resource.ns",
						"id"),
				),
			},
			//
			//
			// Modify each CO's spec with a patch
			{
				Config: testAccResourceKustomizationConfig_crd("test_kustomizations/crd/modified"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(
						"kustomization_resource.clusteredcrd",
						"id"),
					resource.TestCheckResourceAttrSet(
						"kustomization_resource.namespacedcrd",
						"id"),
					resource.TestCheckResourceAttrSet(
						"kustomization_resource.clusteredco",
						"id"),
					resource.TestCheckResourceAttrSet(
						"kustomization_resource.namespacedco",
						"id"),
					resource.TestCheckResourceAttrSet(
						"kustomization_resource.ns",
						"id"),
				),
			},
			//
			//
			// Test state import
			{
				ResourceName:      "kustomization_resource.clusteredcrd",
				ImportStateId:     "apiextensions.k8s.io/CustomResourceDefinition/_/clusteredcrds.test.example.com",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func testAccResourceKustomizationConfig_crd(path string) string {
	return testAccDataSourceKustomizationConfig_basic(path) + `
resource "kustomization_resource" "clusteredcrd" {
	manifest = data.kustomization_build.test.manifests["apiextensions.k8s.io/CustomResourceDefinition/_/clusteredcrds.test.example.com"]
}

resource "kustomization_resource" "namespacedcrd" {
	manifest = data.kustomization_build.test.manifests["apiextensions.k8s.io/CustomResourceDefinition/_/namespacedcrds.test.example.com"]
}

resource "kustomization_resource" "clusteredco" {
	manifest = data.kustomization_build.test.manifests["test.example.com/Clusteredcrd/_/clusteredco"]
}

resource "kustomization_resource" "namespacedco" {
	manifest = data.kustomization_build.test.manifests["test.example.com/Namespacedcrd/test-crd/namespacedco"]
}

resource "kustomization_resource" "ns" {
	manifest = data.kustomization_build.test.manifests["_/Namespace/_/test-crd"]
}
`
}

// Fail namespace not allowed
func TestAccResourceKustomization_failPlanInvalidNamespaceNotAllowed(t *testing.T) {

	resource.Test(t, resource.TestCase{
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			//
			//
			// Expect plan to fail due to namespace not allowed
			{
				Config:      testAccResourceKustomizationConfig_failNamespaceNotAllowed("test_kustomizations/fail_namespace_not_allowed"),
				ExpectError: regexp.MustCompile("Error: github.com/kbst/terraform-provider-kustomize/kustomize.kustomizationResourceDiff: \"rbac.authorization.k8s.io/ClusterRoleBinding/default/invalid\": is not namespace scoped but has metadata.namespace set"),
			},
		},
	})
}

func testAccResourceKustomizationConfig_failNamespaceNotAllowed(path string) string {
	return testAccDataSourceKustomizationConfig_basic(path) + `
resource "kustomization_resource" "crb" {
	manifest = data.kustomization_build.test.manifests["rbac.authorization.k8s.io/ClusterRoleBinding/default/invalid"]
}
`
}

// Fail namespace required
func TestAccResourceKustomization_failNamespaceRequired(t *testing.T) {

	resource.Test(t, resource.TestCase{
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			//
			//
			// Expect plan to fail due to missing namespace
			{
				Config:      testAccResourceKustomizationConfig_failNamespaceRequired("test_kustomizations/fail_namespace_required"),
				ExpectError: regexp.MustCompile("Error: github.com/kbst/terraform-provider-kustomize/kustomize.kustomizationResourceDiff: \"rbac.authorization.k8s.io/RoleBinding/_/invalid\": is namespace scoped and must set metadata.namespace"),
			},
		},
	})
}

func testAccResourceKustomizationConfig_failNamespaceRequired(path string) string {
	return testAccDataSourceKustomizationConfig_basic(path) + `
resource "kustomization_resource" "crb" {
	manifest = data.kustomization_build.test.manifests["rbac.authorization.k8s.io/RoleBinding/_/invalid"]
}
`
}

// Fail plan invalid manifest
func TestAccResourceKustomization_failPlanInvalid(t *testing.T) {

	resource.Test(t, resource.TestCase{
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			//
			//
			// Expect plan to fail due to invalid roleRef
			{
				Config:      testAccResourceKustomizationConfig_failPlanInvalid("test_kustomizations/fail_plan_invalid"),
				ExpectError: regexp.MustCompile("Error: github.com/kbst/terraform-provider-kustomize/kustomize.kustomizationResourceDiff: \"rbac.authorization.k8s.io/ClusterRoleBinding/_/invalid\": ClusterRoleBinding.rbac.authorization.k8s.io \"invalid\" is invalid: roleRef.kind: Unsupported value: \"Role\": supported values: \"ClusterRole\""),
			},
		},
	})
}

func testAccResourceKustomizationConfig_failPlanInvalid(path string) string {
	return testAccDataSourceKustomizationConfig_basic(path) + `
resource "kustomization_resource" "crb" {
	manifest = data.kustomization_build.test.manifests["rbac.authorization.k8s.io/ClusterRoleBinding/_/invalid"]
}
`
}

// Webhook Test
func TestAccResourceKustomization_webhook(t *testing.T) {

	resource.Test(t, resource.TestCase{
		//PreCheck:  func() { testAccPreCheck(t) },
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			//
			//
			// Creating initial webhook
			{
				Config: testAccResourceKustomizationConfig_webhook("test_kustomizations/webhook/initial"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(
						"kustomization_resource.webhook",
						"id"),
				),
			},
			//
			//
			// Applying modified webhook
			{
				Config: testAccResourceKustomizationConfig_webhook("test_kustomizations/webhook/modified"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(
						"kustomization_resource.webhook",
						"id"),
				),
			},
			//
			//
			// Test state import
			{
				ResourceName:      "kustomization_resource.webhook",
				ImportStateId:     "admissionregistration.k8s.io/ValidatingWebhookConfiguration/_/pod-policy.example.com",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func testAccResourceKustomizationConfig_webhook(path string) string {
	return testAccDataSourceKustomizationConfig_basic(path) + `
resource "kustomization_resource" "webhook" {
	manifest = data.kustomization_build.test.manifests["admissionregistration.k8s.io/ValidatingWebhookConfiguration/_/pod-policy.example.com"]
}
`
}

// SA Token Secret
func TestAccResourceKustomization_secretSAToken(t *testing.T) {

	resource.Test(t, resource.TestCase{
		Providers: testAccProviders,
		ExternalProviders: map[string]resource.ExternalProvider{
			// we need to give the K8s GC time to delete the secret
			// while the service account doesn't exist yet
			// https://github.com/kubernetes/kubernetes/issues/109401
			"time": {},
		},
		Steps: []resource.TestStep{
			{
				Config: testAccResourceKustomizationConfig_secretSAToken("test_kustomizations/secret_service_account_token"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("kustomization_resource.ns", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.sec_sa_token", "id"),
					testAccCheckManifestAnnotation("kustomization_resource.sec_sa_token", "kubernetes.io/service-account.name", "test-sa"),
					resource.TestCheckResourceAttrSet("kustomization_resource.sec_default", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.sa", "id"),
				),
			},
		},
	})
}

func testAccResourceKustomizationConfig_secretSAToken(path string) string {
	return testAccDataSourceKustomizationConfig_basic(path) + `
resource "kustomization_resource" "ns" {
	manifest = data.kustomization_build.test.manifests["_/Namespace/_/test-secret-sa-token"]
}

resource "kustomization_resource" "sec_sa_token" {
	manifest = data.kustomization_build.test.manifests["_/Secret/test-secret-sa-token/test-sa-token"]
}

resource "kustomization_resource" "sec_default" {
	manifest = data.kustomization_build.test.manifests["_/Secret/test-secret-sa-token/test"]
}

resource "time_sleep" "garbage_collection" {
	create_duration = "5s"
}

resource "kustomization_resource" "sa" {
	manifest = data.kustomization_build.test.manifests["_/ServiceAccount/test-secret-sa-token/test-sa"]

	depends_on = [time_sleep.garbage_collection]
}
`
}

// TransformerConfigs test
func TestAccResourceKustomization_transformerConfigs(t *testing.T) {

	resource.Test(t, resource.TestCase{
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			//
			//
			// Applying initial config without the test label
			{
				Config: testAccResourceKustomizationConfig_transformerConfigs("test_kustomizations/transformer_configs/initial"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("kustomization_resource.ns", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.svc", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.dep1", "id"),
					testAccCheckManifestLabelAbsent("kustomization_resource.dep1", "test.example.com/test-label"),
					testAccCheckManifestSelectorAbsent("kustomization_resource.dep1", "test.example.com/test-label"),
				),
			},
			//
			//
			// Applying modified config adding the test label
			{
				Config: testAccResourceKustomizationConfig_transformerConfigs("test_kustomizations/transformer_configs/modified"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("kustomization_resource.ns", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.svc", "id"),
					resource.TestCheckResourceAttrSet("kustomization_resource.dep1", "id"),
					testAccCheckManifestLabel("kustomization_resource.dep1", "test.example.com/test-label", "test-value"),
					testAccCheckManifestSelectorAbsent("kustomization_resource.dep1", "test.example.com/test-label"),
				),
			},
		},
	})
}

func testAccResourceKustomizationConfig_transformerConfigs(path string) string {
	return testAccDataSourceKustomizationConfig_basic(path) + `
resource "kustomization_resource" "ns" {
	manifest = data.kustomization_build.test.manifests["_/Namespace/_/test-transformer-config"]
}

resource "kustomization_resource" "svc" {
	manifest = data.kustomization_build.test.manifests["_/Service/test-transformer-config/test"]
}

resource "kustomization_resource" "dep1" {
	manifest = data.kustomization_build.test.manifests["apps/Deployment/test-transformer-config/test"]
}
`
}

//
//
// Test check functions

func testAccCheckDeploymentPurged(n string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		client := testAccProvider.Meta().(*Config).Client

		gvr := k8sschema.GroupVersionResource{
			Group:    "apps",
			Version:  "v1",
			Resource: "deployments",
		}
		namespace := "test"
		name := "test2"

		_, k8serr := client.
			Resource(gvr).
			Namespace(namespace).
			Get(context.TODO(), name, k8smetav1.GetOptions{})
		if k8serr != nil {
			if !k8serrors.IsNotFound(k8serr) {
				return fmt.Errorf("Unexpected error from K8s api: %s", k8serr)
			}
		} else {
			return fmt.Errorf("Resource not purged from K8s api: %s", n)
		}

		return nil
	}
}

func testAccCheckResourceReady(
	n string, namespace string, name string, resourceName string,
	readyCheck readyCheckFunc,
) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		u, err := getResourceFromTestState(s, n)
		if err != nil {
			return err
		}

		resp, err := getResourceFromK8sAPI(u)
		if err != nil {
			return err
		}
		ready, err := readyCheck(resp)
		if err != nil {
			return err
		}
		if !ready {
			return fmt.Errorf("%s %s in %s not ready", resourceName, name, namespace)
		}
		return nil
	}
}

func testAccCheckResourceNotReady(
	n string, namespace string, name string, resourceName string,
	readyCheck readyCheckFunc,
) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		u, err := getResourceFromTestState(s, n)
		if err != nil {
			return err
		}

		resp, err := getResourceFromK8sAPI(u)
		if err != nil {
			return err
		}
		ready, err := readyCheck(resp)
		if err != nil {
			return err
		}
		if ready {
			return fmt.Errorf("%s %s in %s unexpectedly ready", resourceName, name, namespace)
		}
		return nil
	}
}

func getResourceFromTestState(s *terraform.State, n string) (ur *k8sunstructured.Unstructured, err error) {
	rs, ok := s.RootModule().Resources[n]
	if !ok {
		return nil, fmt.Errorf("Not found: %s", n)
	}

	srcJSON := rs.Primary.Attributes["manifest"]
	km := kManifest{}

	err = km.load([]byte(srcJSON))
	if err != nil {
		return nil, err
	}

	return km.resource, nil
}

func getResourceFromK8sAPI(u *k8sunstructured.Unstructured) (resp *k8sunstructured.Unstructured, err error) {
	client := testAccProvider.Meta().(*Config).Client
	mapper := testAccProvider.Meta().(*Config).Mapper

	mapping, err := mapper.RESTMapping(u.GroupVersionKind().GroupKind(), u.GroupVersionKind().Version)
	if err != nil {
		return nil, err
	}
	namespace := u.GetNamespace()
	name := u.GetName()

	resp, err = client.
		Resource(mapping.Resource).
		Namespace(namespace).
		Get(context.TODO(), name, k8smetav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func testAccCheckManifestAnnotation(n string, k string, v string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		u, err := getResourceFromTestState(s, n)
		if err != nil {
			return err
		}

		resp, err := getResourceFromK8sAPI(u)
		if err != nil {
			return err
		}

		annotations := resp.GetAnnotations()
		a, ok := annotations[k]
		if !ok {
			return fmt.Errorf("Annotation missing: %s", k)
		}

		if a != v {
			return fmt.Errorf("Annotation value incorrect: expected %s, got %s", v, a)
		}

		return nil
	}
}

func testAccCheckManifestAnnotationAbsent(n string, k string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		u, err := getResourceFromTestState(s, n)
		if err != nil {
			return err
		}

		resp, err := getResourceFromK8sAPI(u)
		if err != nil {
			return err
		}

		annotations := resp.GetAnnotations()
		_, ok := annotations[k]
		if ok {
			return fmt.Errorf("Unexpected annotation exists: %s", k)
		}

		return nil
	}
}

func testAccCheckManifestLabel(n string, k string, v string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		u, err := getResourceFromTestState(s, n)
		if err != nil {
			return err
		}

		resp, err := getResourceFromK8sAPI(u)
		if err != nil {
			return err
		}

		labels := resp.GetLabels()
		a, ok := labels[k]
		if !ok {
			return fmt.Errorf("Label missing: %s", k)
		}

		if a != v {
			return fmt.Errorf("Label value incorrect: expected %s, got %s", v, a)
		}

		return nil
	}
}

func testAccCheckManifestLabelAbsent(n string, k string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		u, err := getResourceFromTestState(s, n)
		if err != nil {
			return err
		}

		resp, err := getResourceFromK8sAPI(u)
		if err != nil {
			return err
		}

		labels := resp.GetLabels()
		_, ok := labels[k]
		if ok {
			return fmt.Errorf("Unexpected label exists: %s", k)
		}

		return nil
	}
}

func testAccCheckManifestSelector(n string, k string, v string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		u, err := getResourceFromTestState(s, n)
		if err != nil {
			return err
		}

		resp, err := getResourceFromK8sAPI(u)
		if err != nil {
			return err
		}

		matchLabels, ok, err := k8sunstructured.NestedStringMap(resp.Object, "spec", "selector", "matchLabels")
		if !ok {
			return fmt.Errorf("Selector matchLabels missing from spec")
		}
		if err != nil {
			return err
		}

		a, ok := matchLabels[k]
		if !ok {
			return fmt.Errorf("Selector matchLabels missing: %s, %v", k, matchLabels)
		}

		if a != v {
			return fmt.Errorf("Selector matchLabels value incorrect: expected %s, got %s", v, a)
		}

		return nil
	}
}

func testAccCheckManifestSelectorAbsent(n string, k string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		u, err := getResourceFromTestState(s, n)
		if err != nil {
			return err
		}

		resp, err := getResourceFromK8sAPI(u)
		if err != nil {
			return err
		}

		matchLabels, ok, err := k8sunstructured.NestedStringMap(resp.Object, "spec", "selector", "matchLabels")
		if !ok {
			return fmt.Errorf("Selector matchLabels missing from spec")
		}
		if err != nil {
			return err
		}

		_, ok = matchLabels[k]
		if ok {
			return fmt.Errorf("Unexpected selector matchLabels: %s", k)
		}

		return nil
	}
}

func testAccCheckManifestNestedString(n string, expected string, k ...string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		u, err := getResourceFromTestState(s, n)
		if err != nil {
			return err
		}

		resp, err := getResourceFromK8sAPI(u)
		if err != nil {
			return err
		}

		k8spath := strings.Join(k, ".")

		actual, ok, err := k8sunstructured.NestedString(resp.Object, k...)
		if !ok {
			return fmt.Errorf("%s missing from resource %s", k8spath, n)
		}
		if err != nil {
			return err
		}

		if actual != expected {
			return fmt.Errorf("value %s of %s does not match expected %s", actual, k8spath, expected)
		}

		return nil
	}
}

func assertDurationIsLongerThan(start time.Time, duration time.Duration) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		elapsed := time.Since(start)
		if elapsed > duration {
			return nil
		}
		return fmt.Errorf("elapsed time %s is not longer than %s", elapsed, duration)
	}
}

func assertDurationIsShorterThan(start time.Time, duration time.Duration) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		elapsed := time.Since(start)
		if elapsed < duration {
			return nil
		}
		return fmt.Errorf("elapsed time %s is not shorter than %s", elapsed, duration)
	}
}
