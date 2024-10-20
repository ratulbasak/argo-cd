package lua

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/argoproj/gitops-engine/pkg/diff"

	applicationpkg "github.com/argoproj/argo-cd/v2/pkg/apiclient/application"
	appsv1 "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v2/util/cli"
	"github.com/argoproj/argo-cd/v2/util/errors"
)

type testNormalizer struct{}

func (t testNormalizer) Normalize(un *unstructured.Unstructured) error {
	if un == nil {
		return nil
	}
	switch un.GetKind() {
	case "Job":
		err := unstructured.SetNestedField(un.Object, map[string]interface{}{"name": "not sure why this works"}, "metadata")
		if err != nil {
			return fmt.Errorf("failed to normalize Job: %w", err)
		}
	}
	switch un.GetKind() {
	case "DaemonSet", "Deployment", "StatefulSet":
		err := unstructured.SetNestedStringMap(un.Object, map[string]string{"kubectl.kubernetes.io/restartedAt": "0001-01-01T00:00:00Z"}, "spec", "template", "metadata", "annotations")
		if err != nil {
			return fmt.Errorf("failed to normalize %s: %w", un.GetKind(), err)
		}
	}
	switch un.GetKind() {
	case "Deployment":
		err := unstructured.SetNestedField(un.Object, nil, "status")
		if err != nil {
			return fmt.Errorf("failed to normalize %s: %w", un.GetKind(), err)
		}
		err = unstructured.SetNestedField(un.Object, nil, "metadata", "creationTimestamp")
		if err != nil {
			return fmt.Errorf("failed to normalize %s: %w", un.GetKind(), err)
		}
		err = unstructured.SetNestedField(un.Object, nil, "metadata", "generation")
		if err != nil {
			return fmt.Errorf("failed to normalize %s: %w", un.GetKind(), err)
		}
	case "Rollout":
		err := unstructured.SetNestedField(un.Object, nil, "spec", "restartAt")
		if err != nil {
			return fmt.Errorf("failed to normalize %s: %w", un.GetKind(), err)
		}
	case "ExternalSecret", "PushSecret":
		err := unstructured.SetNestedStringMap(un.Object, map[string]string{"force-sync": "0001-01-01T00:00:00Z"}, "metadata", "annotations")
		if err != nil {
			return fmt.Errorf("failed to normalize %s: %w", un.GetKind(), err)
		}
	case "Workflow":
		err := unstructured.SetNestedField(un.Object, nil, "metadata", "resourceVersion")
		if err != nil {
			return fmt.Errorf("failed to normalize Rollout: %w", err)
		}
		err = unstructured.SetNestedField(un.Object, nil, "metadata", "uid")
		if err != nil {
			return fmt.Errorf("failed to normalize Rollout: %w", err)
		}
		err = unstructured.SetNestedField(un.Object, nil, "metadata", "annotations", "workflows.argoproj.io/scheduled-time")
		if err != nil {
			return fmt.Errorf("failed to normalize Rollout: %w", err)
		}
	case "HelmRelease", "ImageRepository", "ImageUpdateAutomation", "Kustomization", "Receiver", "Bucket", "GitRepository", "HelmChart", "HelmRepository", "OCIRepository":
		err := unstructured.SetNestedStringMap(un.Object, map[string]string{"reconcile.fluxcd.io/requestedAt": "By Argo CD at: 0001-01-01T00:00:00"}, "metadata", "annotations")
		if err != nil {
			return fmt.Errorf("failed to normalize %s: %w", un.GetKind(), err)
		}
	}
	return nil
}

type ActionTestStructure struct {
	DiscoveryTests []IndividualDiscoveryTest `yaml:"discoveryTests"`
	ActionTests    []IndividualActionTest    `yaml:"actionTests"`
}

type IndividualDiscoveryTest struct {
	InputPath string                  `yaml:"inputPath"`
	Result    []appsv1.ResourceAction `yaml:"result"`
}

type IndividualActionTest struct {
	Action             string            `yaml:"action"`
	InputPath          string            `yaml:"inputPath"`
	ExpectedOutputPath string            `yaml:"expectedOutputPath"`
	InputStr           string            `yaml:"input"`
	Parameters         map[string]string `yaml:"parameters"`
}

func TestLuaResourceActionsScript(t *testing.T) {
	err := filepath.Walk("../../resource_customizations", func(path string, f os.FileInfo, err error) error {
		if !strings.Contains(path, "action_test.yaml") {
			return nil
		}
		require.NoError(t, err)
		dir := filepath.Dir(path)
		// TODO: Change to path
		yamlBytes, err := os.ReadFile(dir + "/action_test.yaml")
		require.NoError(t, err)
		var resourceTest ActionTestStructure
		err = yaml.Unmarshal(yamlBytes, &resourceTest)
		require.NoError(t, err)
		for i := range resourceTest.DiscoveryTests {
			test := resourceTest.DiscoveryTests[i]
			testName := fmt.Sprintf("discovery/%s", test.InputPath)
			t.Run(testName, func(t *testing.T) {
				vm := VM{
					UseOpenLibs: true,
				}
				obj := getObj(filepath.Join(dir, test.InputPath))
				discoveryLua, err := vm.GetResourceActionDiscovery(obj)
				require.NoError(t, err)
				result, err := vm.ExecuteResourceActionDiscovery(obj, discoveryLua)
				require.NoError(t, err)
				for i := range result {
					assert.Contains(t, test.Result, result[i])
				}
			})
		}
		for i := range resourceTest.ActionTests {
			test := resourceTest.ActionTests[i]
			testName := fmt.Sprintf("actions/%s/%s", test.Action, test.InputPath)

			t.Run(testName, func(t *testing.T) {
				vm := VM{
					// Uncomment the following line if you need to use lua libraries debugging
					// purposes. Otherwise, leave this false to ensure tests reflect the same
					// privileges that API server has.
					// UseOpenLibs: true,
				}
				sourceObj := getObj(filepath.Join(dir, test.InputPath))
				action, err := vm.GetResourceAction(sourceObj, test.Action)

				require.NoError(t, err)

				// Parse action parameters
				var params []*applicationpkg.ResourceActionParameters
				if test.Parameters != nil {
					for k, v := range test.Parameters {
						params = append(params, &applicationpkg.ResourceActionParameters{
							Name:  &k,
							Value: &v,
						})
					}
				}

				require.NoError(t, err)
				impactedResources, err := vm.ExecuteResourceAction(sourceObj, action.ActionLua, params)
				require.NoError(t, err)

				// Treat the Lua expected output as a list
				expectedObjects := getExpectedObjectList(t, filepath.Join(dir, test.ExpectedOutputPath))

				for _, impactedResource := range impactedResources {
					result := impactedResource.UnstructuredObj

					// The expected output is a list of objects
					// Find the actual impacted resource in the expected output
					expectedObj := findFirstMatchingItem(expectedObjects.Items, func(u unstructured.Unstructured) bool {
						// Some resources' name is derived from the source object name, so the returned name is not actually equal to the testdata output name
						// Considering the resource found in the testdata output if its name starts with source object name
						// TODO: maybe this should use a normalizer function instead of hard-coding the resource specifics here
						if (result.GetKind() == "Job" && sourceObj.GetKind() == "CronJob") || (result.GetKind() == "Workflow" && (sourceObj.GetKind() == "CronWorkflow" || sourceObj.GetKind() == "WorkflowTemplate")) {
							return u.GroupVersionKind() == result.GroupVersionKind() && strings.HasPrefix(u.GetName(), sourceObj.GetName()) && u.GetNamespace() == result.GetNamespace()
						} else {
							return u.GroupVersionKind() == result.GroupVersionKind() && u.GetName() == result.GetName() && u.GetNamespace() == result.GetNamespace()
						}
					})

					assert.NotNil(t, expectedObj)

					switch impactedResource.K8SOperation {
					// No default case since a not supported operation would have failed upon unmarshaling earlier
					case PatchOperation:
						// Patching is only allowed for the source resource, so the GVK + name + ns must be the same as the impacted resource
						assert.EqualValues(t, sourceObj.GroupVersionKind(), result.GroupVersionKind())
						assert.EqualValues(t, sourceObj.GetName(), result.GetName())
						assert.EqualValues(t, sourceObj.GetNamespace(), result.GetNamespace())
					case CreateOperation:
						switch result.GetKind() {
						case "Job":
						case "Workflow":
							// The name of the created resource is derived from the source object name, so the returned name is not actually equal to the testdata output name
							result.SetName(expectedObj.GetName())
						}
					}

					// Add specific checks for parameter-based actions
					if test.Action == "scale" && sourceObj.GetKind() == "Deployment" {
						// Check spec.replicas
						specMap, found, err := unstructured.NestedMap(result.Object, "spec")
						if err != nil {
								t.Errorf("Error accessing spec field: %v", err)
						} else if !found {
								t.Errorf("spec not found in actual result. Result object: %+v", result.Object)
						} else {
								t.Logf("Spec field: %+v", specMap)
						}

						if specMap != nil {
								// Try to access replicas directly from the spec map
								replicasRaw, found := specMap["replicas"]
								if !found {
										t.Errorf("replicas field not found in spec. Spec: %+v", specMap)
								} else {
										t.Logf("Replicas field (raw): %v", replicasRaw)
										
										var actualReplicas int64
										switch v := replicasRaw.(type) {
										case int64:
												actualReplicas = v
										case float64:
												actualReplicas = int64(v)
										case int:
												actualReplicas = int64(v)
										default:
												t.Errorf("Unexpected type for replicas: %T", replicasRaw)
										}

										expectedReplicas, err := strconv.ParseInt(test.Parameters["replicas"], 10, 64)
										if err != nil {
												t.Errorf("Error parsing expected replicas: %v", err)
										} else {
												assert.Equal(t, expectedReplicas, actualReplicas, "replica count mismatch")
										}
								}
						}
					}

					// Ideally, we would use a assert.Equal to detect the difference, but the Lua VM returns a object with float64 instead of the original int32.  As a result, the assert.Equal is never true despite that the change has been applied.
					diffResult, err := diff.Diff(expectedObj, result, diff.WithNormalizer(testNormalizer{}))
					require.NoError(t, err)
					if diffResult.Modified {
						t.Error("Output does not match input:")
						err = cli.PrintDiff(test.Action, expectedObj, result)
						require.NoError(t, err)
					}
				}
			})
		}

		return nil
	})
	require.NoError(t, err)
}

// Handling backward compatibility.
// The old-style actions return a single object in the expected output from testdata, so will wrap them in a list
func getExpectedObjectList(t *testing.T, path string) *unstructured.UnstructuredList {
	t.Helper()
	yamlBytes, err := os.ReadFile(path)
	errors.CheckError(err)
	unstructuredList := &unstructured.UnstructuredList{}
	yamlString := bytes.NewBuffer(yamlBytes).String()
	if yamlString[0] == '-' {
		// The string represents a new-style action array output, where each member is a wrapper around a k8s unstructured resource
		objList := make([]map[string]interface{}, 5)
		err = yaml.Unmarshal(yamlBytes, &objList)
		errors.CheckError(err)
		unstructuredList.Items = make([]unstructured.Unstructured, len(objList))
		// Append each map in objList to the Items field of the new object
		for i, obj := range objList {
			unstructuredObj, ok := obj["unstructuredObj"].(map[string]interface{})
			if !ok {
				t.Error("Wrong type of unstructuredObj")
			}
			unstructuredList.Items[i] = unstructured.Unstructured{Object: unstructuredObj}
		}
	} else {
		// The string represents an old-style action object output, which is a k8s unstructured resource
		obj := make(map[string]interface{})
		err = yaml.Unmarshal(yamlBytes, &obj)
		errors.CheckError(err)
		unstructuredList.Items = make([]unstructured.Unstructured, 1)
		unstructuredList.Items[0] = unstructured.Unstructured{Object: obj}
	}
	return unstructuredList
}

func findFirstMatchingItem(items []unstructured.Unstructured, f func(unstructured.Unstructured) bool) *unstructured.Unstructured {
	var matching *unstructured.Unstructured = nil
	for _, item := range items {
		if f(item) {
			matching = &item
			break
		}
	}
	return matching
}
