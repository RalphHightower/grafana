package provisioning

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/grafana/grafana/pkg/apimachinery/utils"
	provisioning "github.com/grafana/grafana/pkg/apis/provisioning/v0alpha1"
	"github.com/grafana/grafana/pkg/extensions"
	"github.com/grafana/grafana/pkg/infra/usagestats"
	"github.com/grafana/grafana/pkg/tests/apis"
)

// printFileTree prints the directory structure as a tree for debugging purposes
func printFileTree(t *testing.T, rootPath string) {
	t.Helper()
	t.Logf("File tree for %s:", rootPath)

	err := filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(rootPath, path)
		if err != nil {
			return err
		}

		if relPath == "." {
			return nil
		}

		depth := strings.Count(relPath, string(filepath.Separator))
		indent := strings.Repeat("  ", depth)

		if d.IsDir() {
			t.Logf("%s├── %s/", indent, d.Name())
		} else {
			info, err := d.Info()
			if err != nil {
				t.Logf("%s├── %s (error reading info)", indent, d.Name())
			} else {
				t.Logf("%s├── %s (%d bytes)", indent, d.Name(), info.Size())
			}
		}

		return nil
	})
	if err != nil {
		t.Logf("Error walking directory: %v", err)
	}
}

func TestIntegrationProvisioning_CreatingAndGetting(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	helper := runGrafana(t)
	createOptions := metav1.CreateOptions{FieldValidation: "Strict"}
	ctx := context.Background()

	inputFiles := []string{
		"testdata/github-readonly.json.tmpl",
		"testdata/local-readonly.json.tmpl",
	}

	for _, inputFilePath := range inputFiles {
		t.Run(inputFilePath, func(t *testing.T) {
			input := helper.RenderObject(t, inputFilePath, nil)

			_, err := helper.Repositories.Resource.Create(ctx, input, createOptions)
			require.NoError(t, err, "failed to create resource")

			name := mustNestedString(input.Object, "metadata", "name")
			output, err := helper.Repositories.Resource.Get(ctx, name, metav1.GetOptions{})
			require.NoError(t, err, "failed to read back resource")

			// Move encrypted token mutation
			token, found, err := unstructured.NestedString(output.Object, "spec", "github", "encryptedToken")
			require.NoError(t, err, "encryptedToken is not a string")
			if found {
				unstructured.RemoveNestedField(input.Object, "spec", "github", "token")
				err = unstructured.SetNestedField(input.Object, token, "spec", "github", "encryptedToken")
				require.NoError(t, err, "unable to copy encrypted token")
			}

			// Marshal as real objects to ",omitempty" values are tested properly
			expectedRepo := unstructuredToRepository(t, input)
			returnedRepo := unstructuredToRepository(t, output)
			require.Equal(t, expectedRepo.Spec, returnedRepo.Spec)

			// A viewer should not be able to see the same thing
			var statusCode int
			rsp := helper.ViewerREST.Get().
				Namespace("default").
				Resource("repositories").
				Name(name).
				Do(context.Background())
			require.Error(t, rsp.Error())
			rsp.StatusCode(&statusCode)
			require.Equal(t, http.StatusForbidden, statusCode)

			// Viewer can see file listing
			rsp = helper.AdminREST.Get().
				Namespace("default").
				Resource("repositories").
				Name(name).
				Suffix("files/").
				Do(context.Background())
			require.NoError(t, rsp.Error())

			// Verify that we can list refs
			rsp = helper.AdminREST.Get().
				Namespace("default").
				Resource("repositories").
				Name(name).
				Suffix("refs").
				Do(context.Background())

			if expectedRepo.Spec.Type == provisioning.LocalRepositoryType {
				require.ErrorContains(t, rsp.Error(), "does not support versioned operations")
			} else {
				require.NoError(t, rsp.Error())
				refs := &provisioning.RefList{}
				err = rsp.Into(refs)
				require.NoError(t, err)
				require.True(t, len(refs.Items) >= 1, "should have at least one ref")

				var foundBranch bool
				for _, ref := range refs.Items {
					// FIXME: this assertion should be improved for all git types and take things from config
					if ref.Name == "integration-test" {
						require.Equal(t, "0f3370c212b04b9704e00f6926ef339bf91c7a1b", ref.Hash)
						require.Equal(t, "https://github.com/grafana/grafana-git-sync-demo/tree/integration-test", ref.RefURL)
						foundBranch = true
					}
				}

				require.True(t, foundBranch, "branch should be found")
			}
		})
	}

	// Viewer can see settings listing
	t.Run("viewer has access to list", func(t *testing.T) {
		settings := &provisioning.RepositoryViewList{}
		rsp := helper.ViewerREST.Get().
			Namespace("default").
			Suffix("settings").
			Do(context.Background())
		require.NoError(t, rsp.Error())
		err := rsp.Into(settings)
		require.NoError(t, err)
		require.Len(t, settings.Items, len(inputFiles))

		// FIXME: this should be an enterprise integration test
		if extensions.IsEnterprise {
			require.ElementsMatch(t, []provisioning.RepositoryType{
				provisioning.LocalRepositoryType,
				provisioning.GitHubRepositoryType,
				provisioning.GitRepositoryType,
				provisioning.BitbucketRepositoryType,
				provisioning.GitLabRepositoryType,
			}, settings.AvailableRepositoryTypes)
		} else {
			require.ElementsMatch(t, []provisioning.RepositoryType{
				provisioning.LocalRepositoryType,
				provisioning.GitHubRepositoryType,
			}, settings.AvailableRepositoryTypes)
		}
	})

	t.Run("Repositories are reported in stats", func(t *testing.T) {
		report := apis.DoRequest(helper.K8sTestHelper, apis.RequestParams{
			Method: http.MethodGet,
			Path:   "/api/admin/usage-report-preview",
			User:   helper.Org1.Admin,
		}, &usagestats.Report{})

		stats := map[string]any{}
		for k, v := range report.Result.Metrics {
			if strings.HasPrefix(k, "stats.repository.") {
				stats[k] = v
			}
		}
		require.Equal(t, map[string]any{
			"stats.repository.github.count": 1.0,
			"stats.repository.local.count":  1.0,
		}, stats)
	})
}

func TestIntegrationProvisioning_FailInvalidSchema(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	t.Skip("Reenable this test once we enforce schema validation for provisioning")

	helper := runGrafana(t)
	ctx := context.Background()

	const repo = "invalid-schema-tmp"
	// Set up the repository and the file to import.
	helper.CopyToProvisioningPath(t, "testdata/invalid-dashboard-schema.json", "invalid-dashboard-schema.json")

	localTmp := helper.RenderObject(t, "testdata/local-write.json.tmpl", map[string]any{
		"Name":        repo,
		"SyncEnabled": true,
	})
	_, err := helper.Repositories.Resource.Create(ctx, localTmp, metav1.CreateOptions{})
	require.NoError(t, err)

	// Make sure the repo can read and validate the file
	_, err = helper.Repositories.Resource.Get(ctx, repo, metav1.GetOptions{}, "files", "invalid-dashboard-schema.json")
	status := helper.RequireApiErrorStatus(err, metav1.StatusReasonBadRequest, http.StatusBadRequest)
	require.Equal(t, status.Message, "Dry run failed: Dashboard.dashboard.grafana.app \"invalid-schema-uid\" is invalid: [spec.panels.0.repeatDirection: Invalid value: conflicting values \"h\" and \"this is not an allowed value\", spec.panels.0.repeatDirection: Invalid value: conflicting values \"v\" and \"this is not an allowed value\"]")

	const invalidSchemaUid = "invalid-schema-uid"
	_, err = helper.DashboardsV1.Resource.Get(ctx, invalidSchemaUid, metav1.GetOptions{})
	require.Error(t, err, "invalid dashboard shouldn't exist")
	require.True(t, apierrors.IsNotFound(err))

	var jobObj *unstructured.Unstructured
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		result := helper.AdminREST.Post().
			Namespace("default").
			Resource("repositories").
			Name(repo).
			SubResource("jobs").
			Body(asJSON(&provisioning.JobSpec{
				Action: provisioning.JobActionPull,
				Pull:   &provisioning.SyncJobOptions{},
			})).
			SetHeader("Content-Type", "application/json").
			Do(t.Context())
		require.NoError(collect, result.Error())
		job, err := result.Get()
		require.NoError(collect, err)
		var ok bool
		jobObj, ok = job.(*unstructured.Unstructured)
		assert.True(collect, ok, "expecting unstructured object, but got %T", job)
	}, time.Second*10, time.Millisecond*10, "Expected to be able to start a sync job")

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		// helper.TriggerJobProcessing(t)
		result, err := helper.Repositories.Resource.Get(ctx, repo, metav1.GetOptions{},
			"jobs", string(jobObj.GetUID()))

		if apierrors.IsNotFound(err) {
			assert.Fail(collect, "job '%s' not found yet yet", jobObj.GetName())
			return // continue trying
		}

		// Can fail fast here -- the jobs are immutable
		require.NoError(t, err)
		require.NotNil(t, result)

		job := &provisioning.Job{}
		err = runtime.DefaultUnstructuredConverter.FromUnstructured(result.Object, job)
		require.NoError(t, err, "should convert to Job object")

		assert.Equal(t, provisioning.JobStateError, job.Status.State)
		assert.Equal(t, job.Status.Message, "completed with errors")
		assert.Equal(t, job.Status.Errors[0], "Dashboard.dashboard.grafana.app \"invalid-schema-uid\" is invalid: [spec.panels.0.repeatDirection: Invalid value: conflicting values \"h\" and \"this is not an allowed value\", spec.panels.0.repeatDirection: Invalid value: conflicting values \"v\" and \"this is not an allowed value\"]")
	}, time.Second*10, time.Millisecond*10, "Expected provisioning job to conclude with the status failed")

	_, err = helper.DashboardsV1.Resource.Get(ctx, invalidSchemaUid, metav1.GetOptions{})
	require.Error(t, err, "invalid dashboard shouldn't have been created")
	require.True(t, apierrors.IsNotFound(err))

	err = helper.Repositories.Resource.Delete(ctx, repo, metav1.DeleteOptions{}, "files", "invalid-dashboard-schema.json")
	require.NoError(t, err, "should delete the resource file")
}

func TestIntegrationProvisioning_CreatingGitHubRepository(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	helper := runGrafana(t)
	ctx := context.Background()

	// FIXME: instead of using an existing GitHub repository, we should create a new one for the tests and a branch
	// This was the previous structure
	// ghmock.WithRequestMatchHandler(ghmock.GetReposGitTreesByOwnerByRepoByTreeSha,
	// 	ghHandleTree(t, map[string][]*gh.TreeEntry{
	// 		"deadbeef": {
	// 			treeEntryDir("grafana", "subtree"),
	// 		},
	// 		"subtree": {
	// 			treeEntry("dashboard.json", helper.LoadFile("testdata/all-panels.json")),
	// 			treeEntryDir("subdir", "subtree2"),
	// 			treeEntry("subdir/dashboard2.yaml", helper.LoadFile("testdata/text-options.json")),
	// 		},
	// 	})),

	// FIXME: uncomment these to implement webhook integration tests.
	// helper.GetEnv().GitHubFactory.Client = ghmock.NewMockedHTTPClient(
	// 	ghmock.WithRequestMatchHandler(ghmock.GetReposHooksByOwnerByRepo, ghAlwaysWrite(t, []*gh.Hook{})),
	// 	ghmock.WithRequestMatchHandler(ghmock.PostReposHooksByOwnerByRepo, ghAlwaysWrite(t, &gh.Hook{ID: gh.Ptr(int64(123))})),
	// )

	const repo = "github-create-test"
	_, err := helper.Repositories.Resource.Create(ctx,
		helper.RenderObject(t, "testdata/github-readonly.json.tmpl", map[string]any{
			"Name":        repo,
			"SyncEnabled": true,
			"SyncTarget":  "instance",
			"Path":        "grafana/",
		}),
		metav1.CreateOptions{},
	)
	require.NoError(t, err)

	helper.SyncAndWait(t, repo, nil)

	// By now, we should have synced, meaning we have data to read in the local Grafana instance!

	found, err := helper.DashboardsV1.Resource.List(ctx, metav1.ListOptions{})
	require.NoError(t, err, "can list values")

	names := []string{}
	for _, v := range found.Items {
		names = append(names, v.GetName())
	}
	require.Len(t, names, 3, "should have three dashboards")
	assert.Contains(t, names, "adg5vbj", "should contain dashboard.json's contents")
	assert.Contains(t, names, "admfz74", "should contain dashboard2.yaml's contents")
	assert.Contains(t, names, "adn5mxb", "should contain dashboard2.yaml's contents")

	err = helper.Repositories.Resource.Delete(ctx, repo, metav1.DeleteOptions{})
	require.NoError(t, err, "should delete values")

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		found, err := helper.DashboardsV1.Resource.List(ctx, metav1.ListOptions{})
		assert.NoError(t, err, "can list values")
		assert.Equal(collect, 0, len(found.Items), "expected dashboards to be deleted")
	}, time.Second*20, time.Millisecond*10, "Expected dashboards to be deleted")

	t.Run("github url cleanup", func(t *testing.T) {
		tests := []struct {
			name   string
			input  string
			output string
		}{
			{
				name:   "simple-url",
				input:  "https://github.com/dprokop/grafana-git-sync-test",
				output: "https://github.com/dprokop/grafana-git-sync-test",
			},
			{
				name:   "trim-dot-git",
				input:  "https://github.com/dprokop/grafana-git-sync-test.git",
				output: "https://github.com/dprokop/grafana-git-sync-test",
			},
			{
				name:   "trim-slash",
				input:  "https://github.com/dprokop/grafana-git-sync-test/",
				output: "https://github.com/dprokop/grafana-git-sync-test",
			},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				input := helper.RenderObject(t, "testdata/github-readonly.json.tmpl", map[string]any{
					"Name": test.name,
					"URL":  test.input,
				})

				_, err := helper.Repositories.Resource.Create(ctx, input, metav1.CreateOptions{})
				require.NoError(t, err, "failed to create resource")

				obj, err := helper.Repositories.Resource.Get(ctx, test.name, metav1.GetOptions{})
				require.NoError(t, err, "failed to read back resource")

				url, _, err := unstructured.NestedString(obj.Object, "spec", "github", "url")
				require.NoError(t, err, "failed to read URL")
				require.Equal(t, test.output, url)

				err = helper.Repositories.Resource.Delete(ctx, test.name, metav1.DeleteOptions{})
				require.NoError(t, err, "failed to delete")
			})
		}
	})
}

func TestIntegrationProvisioning_RunLocalRepository(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	helper := runGrafana(t)
	ctx := context.Background()

	const allPanels = "n1jR8vnnz"
	const repo = "local-local-examples"
	const targetPath = "all-panels.json"

	// Set up the repository.
	localTmp := helper.RenderObject(t, "testdata/local-write.json.tmpl", map[string]any{"Name": repo})
	obj, err := helper.Repositories.Resource.Create(ctx, localTmp, metav1.CreateOptions{})
	require.NoError(t, err)
	name, _, _ := unstructured.NestedString(obj.Object, "metadata", "name")
	require.Equal(t, repo, name, "wrote the expected name")

	// Write a file -- this will create it *both* in the local file system, and in grafana
	t.Run("write all panels", func(t *testing.T) {
		code := 0

		// Check that we can not (yet) UPDATE the target path
		result := helper.AdminREST.Put().
			Namespace("default").
			Resource("repositories").
			Name(repo).
			SubResource("files", targetPath).
			Body(helper.LoadFile("testdata/all-panels.json")).
			SetHeader("Content-Type", "application/json").
			Do(ctx).StatusCode(&code)
		require.Equal(t, http.StatusNotFound, code)
		require.True(t, apierrors.IsNotFound(result.Error()))

		// Now try again with POST (as an editor)
		result = helper.EditorREST.Post().
			Namespace("default").
			Resource("repositories").
			Name(repo).
			SubResource("files", targetPath).
			Body(helper.LoadFile("testdata/all-panels.json")).
			SetHeader("Content-Type", "application/json").
			Do(ctx).StatusCode(&code)
		require.NoError(t, result.Error(), "expecting to be able to create file")
		wrapper := &provisioning.ResourceWrapper{}
		raw, err := result.Raw()
		require.NoError(t, err)
		err = json.Unmarshal(raw, wrapper)
		require.NoError(t, err)
		require.Equal(t, 200, code, "expected 200 response")
		require.Equal(t, provisioning.ClassicDashboard, wrapper.Resource.Type.Classic)
		name, _, _ := unstructured.NestedString(wrapper.Resource.File.Object, "metadata", "name")
		require.Equal(t, allPanels, name, "name from classic UID")
		name, _, _ = unstructured.NestedString(wrapper.Resource.Upsert.Object, "metadata", "name")
		require.Equal(t, allPanels, name, "save the name from the request")

		// Get the file from the grafana database
		obj, err := helper.DashboardsV1.Resource.Get(ctx, allPanels, metav1.GetOptions{})
		require.NoError(t, err, "the value should be saved in grafana")
		val, _, _ := unstructured.NestedString(obj.Object, "metadata", "annotations", utils.AnnoKeyManagerKind)
		require.Equal(t, string(utils.ManagerKindRepo), val, "should have repo annotations")
		val, _, _ = unstructured.NestedString(obj.Object, "metadata", "annotations", utils.AnnoKeyManagerIdentity)
		require.Equal(t, repo, val, "should have repo annotations")

		// Read the file we wrote
		wrapObj, err := helper.Repositories.Resource.Get(ctx, repo, metav1.GetOptions{}, "files", targetPath)
		require.NoError(t, err, "read value")

		wrap := &unstructured.Unstructured{}
		wrap.Object, _, err = unstructured.NestedMap(wrapObj.Object, "resource", "dryRun")
		require.NoError(t, err)
		meta, err := utils.MetaAccessor(wrap)
		require.NoError(t, err)
		require.Equal(t, allPanels, meta.GetName(), "read the name out of the saved file")

		// Check that an admin can update
		meta.SetAnnotation("test", "from-provisioning")
		body, err := json.Marshal(wrap.Object)
		require.NoError(t, err)
		result = helper.AdminREST.Put().
			Namespace("default").
			Resource("repositories").
			Name(repo).
			SubResource("files", targetPath).
			Body(body).
			SetHeader("Content-Type", "application/json").
			Do(ctx).StatusCode(&code)
		require.Equal(t, 200, code)
		require.NoError(t, result.Error(), "update as admin value")
		raw, err = result.Raw()
		require.NoError(t, err)
		err = json.Unmarshal(raw, wrapper)
		require.NoError(t, err)
		anno, _, _ := unstructured.NestedString(wrapper.Resource.File.Object, "metadata", "annotations", "test")
		require.Equal(t, "from-provisioning", anno, "should set the annotation")

		// But a viewer can not
		result = helper.ViewerREST.Put().
			Namespace("default").
			Resource("repositories").
			Name(repo).
			SubResource("files", targetPath).
			Body(body).
			SetHeader("Content-Type", "application/json").
			Do(ctx).StatusCode(&code)
		require.Equal(t, 403, code)
		require.True(t, apierrors.IsForbidden(result.Error()), code)
	})

	t.Run("fail using invalid paths", func(t *testing.T) {
		result := helper.AdminREST.Post().
			Namespace("default").
			Resource("repositories").
			Name(repo).
			SubResource("files", "test", "..", "..", "all-panels.json"). // UNSAFE PATH
			Body(helper.LoadFile("testdata/all-panels.json")).
			SetHeader("Content-Type", "application/json").
			Do(ctx)
		require.Error(t, result.Error(), "invalid path should return error")

		// Read a file with a bad path
		_, err = helper.Repositories.Resource.Get(ctx, repo, metav1.GetOptions{}, "files", "../../all-panels.json")
		require.Error(t, err, "invalid path should error")
	})

	t.Run("require name or generateName", func(t *testing.T) {
		code := 0
		result := helper.AdminREST.Post().
			Namespace("default").
			Resource("repositories").
			Name(repo).
			SubResource("files", "example.json").
			Body([]byte(`apiVersion: dashboard.grafana.app/v0alpha1
kind: Dashboard
spec:
  title: Test dashboard
`)).Do(ctx).StatusCode(&code)
		require.Error(t, result.Error(), "missing name")

		result = helper.AdminREST.Post().
			Namespace("default").
			Resource("repositories").
			Name(repo).
			SubResource("files", "example.json").
			Body([]byte(`apiVersion: dashboard.grafana.app/v0alpha1
kind: Dashboard
metadata:
  generateName: prefix-
spec:
  title: Test dashboard
`)).Do(ctx).StatusCode(&code)
		require.NoError(t, result.Error(), "should create name")
		require.Equal(t, 200, code, "expect OK result")

		raw, err := result.Raw()
		require.NoError(t, err)

		obj := &unstructured.Unstructured{}
		err = json.Unmarshal(raw, obj)
		require.NoError(t, err)

		name, _, _ = unstructured.NestedString(obj.Object, "resource", "upsert", "metadata", "name")
		require.True(t, strings.HasPrefix(name, "prefix-"), "should generate name")
	})
}

func TestIntegrationProvisioning_ImportAllPanelsFromLocalRepository(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	helper := runGrafana(t)
	ctx := context.Background()

	// The dashboard shouldn't exist yet
	const allPanels = "n1jR8vnnz"
	_, err := helper.DashboardsV1.Resource.Get(ctx, allPanels, metav1.GetOptions{})
	require.Error(t, err, "no all-panels dashboard should exist")
	require.True(t, apierrors.IsNotFound(err))

	const repo = "local-tmp"
	// Set up the repository and the file to import.
	helper.CopyToProvisioningPath(t, "testdata/all-panels.json", "all-panels.json")
	localTmp := helper.RenderObject(t, "testdata/local-write.json.tmpl", map[string]any{
		"Name":        repo,
		"SyncEnabled": true,
	})

	// We create the repository
	_, err = helper.Repositories.Resource.Create(ctx, localTmp, metav1.CreateOptions{})
	require.NoError(t, err)

	// Now, we import it, such that it may exist
	// The sync may not be necessary as the sync may have happened automatically at this point
	helper.SyncAndWait(t, repo, nil)

	// Make sure the repo can read and validate the file
	obj, err := helper.Repositories.Resource.Get(ctx, repo, metav1.GetOptions{}, "files", "all-panels.json")
	require.NoError(t, err, "valid path should be fine")

	resource, _, err := unstructured.NestedMap(obj.Object, "resource")
	require.NoError(t, err, "missing resource")
	require.NoError(t, err, "invalid action")
	require.NotNil(t, resource["file"], "the raw file")
	require.NotNil(t, resource["dryRun"], "dryRun result")

	action, _, err := unstructured.NestedString(resource, "action")
	require.NoError(t, err, "invalid action")
	// FIXME: there is no point in in returning action for a read / get request.
	require.Equal(t, "update", action)

	_, err = helper.DashboardsV1.Resource.List(ctx, metav1.ListOptions{})
	require.NoError(t, err, "can list values")

	obj, err = helper.DashboardsV1.Resource.Get(ctx, allPanels, metav1.GetOptions{})
	require.NoError(t, err, "all-panels dashboard should exist")
	require.Equal(t, repo, obj.GetAnnotations()[utils.AnnoKeyManagerIdentity])

	// Try writing the value directly
	err = unstructured.SetNestedField(obj.Object, []any{"aaa", "bbb"}, "spec", "tags")
	require.NoError(t, err, "set tags")
	obj, err = helper.DashboardsV1.Resource.Update(ctx, obj, metav1.UpdateOptions{})
	require.NoError(t, err)
	v, _, _ := unstructured.NestedString(obj.Object, "metadata", "annotations", utils.AnnoKeyUpdatedBy)
	require.Equal(t, "access-policy:provisioning", v)

	// Should not be able to directly delete the managed resource
	err = helper.DashboardsV1.Resource.Delete(ctx, allPanels, metav1.DeleteOptions{})
	require.NoError(t, err, "user can delete")

	_, err = helper.DashboardsV1.Resource.Get(ctx, allPanels, metav1.GetOptions{})
	require.Error(t, err, "should delete the internal resource")
	require.True(t, apierrors.IsNotFound(err))
}

func TestProvisioning_ExportUnifiedToRepository(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	helper := runGrafana(t)
	ctx := context.Background()

	// Write dashboards at
	dashboard := helper.LoadYAMLOrJSONFile("exportunifiedtorepository/dashboard-test-v0.yaml")
	_, err := helper.DashboardsV0.Resource.Create(ctx, dashboard, metav1.CreateOptions{})
	require.NoError(t, err, "should be able to create v0 dashboard")

	dashboard = helper.LoadYAMLOrJSONFile("exportunifiedtorepository/dashboard-test-v1.yaml")
	_, err = helper.DashboardsV1.Resource.Create(ctx, dashboard, metav1.CreateOptions{})
	require.NoError(t, err, "should be able to create v1 dashboard")

	dashboard = helper.LoadYAMLOrJSONFile("exportunifiedtorepository/dashboard-test-v2alpha1.yaml")
	_, err = helper.DashboardsV2alpha1.Resource.Create(ctx, dashboard, metav1.CreateOptions{})
	require.NoError(t, err, "should be able to create v2alpha1 dashboard")

	dashboard = helper.LoadYAMLOrJSONFile("exportunifiedtorepository/dashboard-test-v2alpha2.yaml")
	_, err = helper.DashboardsV2alpha2.Resource.Create(ctx, dashboard, metav1.CreateOptions{})
	require.NoError(t, err, "should be able to create v2alpha2 dashboard")

	// Now for the repository.
	const repo = "local-repository"
	createBody := helper.RenderObject(t, "exportunifiedtorepository/repository.json.tmpl", map[string]any{"Name": repo})
	_, err = helper.Repositories.Resource.Create(ctx, createBody, metav1.CreateOptions{})
	require.NoError(t, err, "should be able to create repository")

	// Now export...
	result := helper.AdminREST.Post().
		Namespace("default").
		Resource("repositories").
		Name(repo).
		SubResource("jobs").
		SetHeader("Content-Type", "application/json").
		Body(asJSON(&provisioning.JobSpec{
			Push: &provisioning.ExportJobOptions{
				Folder: "", // export entire instance
				Path:   "", // no prefix necessary for testing
			},
		})).
		Do(ctx)
	require.NoError(t, result.Error())

	// And time to assert.
	helper.AwaitJobs(t, repo)

	type props struct {
		title      string
		apiVersion string
		name       string
		fileName   string
	}

	printFileTree(t, helper.ProvisioningPath)

	// Check that each file was exported with its stored version
	for _, test := range []props{
		{title: "Test dashboard. Created at v0", apiVersion: "dashboard.grafana.app/v0alpha1", name: "test-v0", fileName: "test-dashboard-created-at-v0.json"},
		{title: "Test dashboard. Created at v1", apiVersion: "dashboard.grafana.app/v1beta1", name: "test-v1", fileName: "test-dashboard-created-at-v1.json"},
		{title: "Test dashboard. Created at v2alpha1", apiVersion: "dashboard.grafana.app/v2alpha1", name: "test-v2alpha1", fileName: "test-dashboard-created-at-v2alpha1.json"},
		{title: "Test dashboard. Created at v2alpha2", apiVersion: "dashboard.grafana.app/v2alpha2", name: "test-v2alpha2", fileName: "test-dashboard-created-at-v2alpha2.json"},
	} {
		fpath := filepath.Join(helper.ProvisioningPath, test.fileName)
		//nolint:gosec // we are ok with reading files in testdata
		body, err := os.ReadFile(fpath)
		require.NoError(t, err, "exported file was not created at path %s", fpath)
		obj := map[string]any{}
		err = json.Unmarshal(body, &obj)
		require.NoError(t, err, "exported file not json %s", fpath)

		val, _, err := unstructured.NestedString(obj, "apiVersion")
		require.NoError(t, err)
		require.Equal(t, test.apiVersion, val)

		val, _, err = unstructured.NestedString(obj, "spec", "title")
		require.NoError(t, err)
		require.Equal(t, test.title, val)

		val, _, err = unstructured.NestedString(obj, "metadata", "name")
		require.NoError(t, err)
		require.Equal(t, test.name, val)

		require.Nil(t, obj["status"], "should not have a status element")
	}
}

func TestIntegrationProvisioning_DeleteResources(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	helper := runGrafana(t)
	ctx := context.Background()

	const repo = "delete-test-repo"
	localTmp := helper.RenderObject(t, "testdata/local-write.json.tmpl", map[string]any{
		"Name":        repo,
		"SyncEnabled": true,
		"SyncTarget":  "instance",
	})
	_, err := helper.Repositories.Resource.Create(ctx, localTmp, metav1.CreateOptions{})
	require.NoError(t, err)

	// Copy the dashboards to the repository path
	helper.CopyToProvisioningPath(t, "testdata/all-panels.json", "dashboard1.json")
	helper.CopyToProvisioningPath(t, "testdata/text-options.json", "folder/dashboard2.json")
	helper.CopyToProvisioningPath(t, "testdata/timeline-demo.json", "folder/nested/dashboard3.json")
	// make sure we don't fail when there is a .keep file in a folder
	helper.CopyToProvisioningPath(t, "testdata/.keep", "folder/nested/.keep")

	// Trigger and wait for a sync job to finish
	helper.SyncAndWait(t, repo, nil)

	dashboards, err := helper.DashboardsV1.Resource.List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Equal(t, 3, len(dashboards.Items))

	folders, err := helper.Folders.Resource.List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Equal(t, 2, len(folders.Items))

	t.Run("delete individual dashboard file, should delete from repo and grafana", func(t *testing.T) {
		result := helper.AdminREST.Delete().
			Namespace("default").
			Resource("repositories").
			Name(repo).
			SubResource("files", "dashboard1.json").
			Do(ctx)
		require.NoError(t, result.Error())
		_, err = helper.Repositories.Resource.Get(ctx, repo, metav1.GetOptions{}, "files", "dashboard1.json")
		require.Error(t, err)
		dashboards, err = helper.DashboardsV1.Resource.List(ctx, metav1.ListOptions{})
		require.NoError(t, err)
		require.Equal(t, 2, len(dashboards.Items))
	})

	t.Run("delete folder, should delete from repo and grafana all nested resources too", func(t *testing.T) {
		// need to delete directly through the url, because the k8s client doesn't support `/` in a subresource
		// but that is needed by gitsync to know that it is a folder
		addr := helper.GetEnv().Server.HTTPServer.Listener.Addr().String()
		url := fmt.Sprintf("http://admin:admin@%s/apis/provisioning.grafana.app/v0alpha1/namespaces/default/repositories/%s/files/folder/", addr, repo)
		req, err := http.NewRequest(http.MethodDelete, url, nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		// nolint:errcheck
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		// should be deleted from the repo
		_, err = helper.Repositories.Resource.Get(ctx, repo, metav1.GetOptions{}, "files", "folder")
		require.Error(t, err)
		_, err = helper.Repositories.Resource.Get(ctx, repo, metav1.GetOptions{}, "files", "folder", "dashboard2.json")
		require.Error(t, err)
		_, err = helper.Repositories.Resource.Get(ctx, repo, metav1.GetOptions{}, "files", "folder", "nested")
		require.Error(t, err)
		_, err = helper.Repositories.Resource.Get(ctx, repo, metav1.GetOptions{}, "files", "folder", "nested", "dashboard3.json")
		require.Error(t, err)

		// all should be deleted from grafana
		for _, d := range dashboards.Items {
			_, err = helper.DashboardsV1.Resource.Get(ctx, d.GetName(), metav1.GetOptions{})
			require.Error(t, err)
		}
		for _, f := range folders.Items {
			_, err = helper.Folders.Resource.Get(ctx, f.GetName(), metav1.GetOptions{})
			require.Error(t, err)
		}
	})

	t.Run("deleting a non-existent file should fail", func(t *testing.T) {
		result := helper.AdminREST.Delete().
			Namespace("default").
			Resource("repositories").
			Name(repo).
			SubResource("files", "non-existent.json").
			Do(ctx)
		require.Error(t, result.Error())
	})
}
