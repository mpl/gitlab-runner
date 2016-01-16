package network

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Sirupsen/logrus"
	. "gitlab.com/gitlab-org/gitlab-ci-multi-runner/common"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
)

const clientError = -100

type GitLabClient struct {
	clients map[string]*client
}

func (n *GitLabClient) getClient(runner RunnerCredentials) (c *client, err error) {
	if n.clients == nil {
		n.clients = make(map[string]*client)
	}
	key := fmt.Sprintf("%s_%s", runner.URL, runner.TLSCAFile)
	c = n.clients[key]
	if c == nil {
		c, err = newClient(runner)
		if err != nil {
			return
		}
		n.clients[key] = c
	}
	return
}

func (n *GitLabClient) getRunnerVersion(config RunnerConfig) VersionInfo {
	info := VersionInfo{
		Name:         NAME,
		Version:      VERSION,
		Revision:     REVISION,
		Platform:     runtime.GOOS,
		Architecture: runtime.GOARCH,
		Executor:     config.Executor,
	}

	if executor := GetExecutor(config.Executor); executor != nil {
		executor.GetFeatures(&info.Features)
	}

	if config.Shell != nil {
		if shell := GetShell(*config.Shell); shell != nil {
			shell.GetFeatures(&info.Features)
		}
	}

	return info
}

func (n *GitLabClient) doRaw(runner RunnerCredentials, method, uri string, statusCode int, request io.Reader, requestType string, response interface{}, headers http.Header) (int, string, string) {
	c, err := n.getClient(runner)
	if err != nil {
		return clientError, err.Error(), ""
	}

	return c.do(uri, method, statusCode, request, requestType, response, headers)
}

func (n *GitLabClient) doJson(runner RunnerCredentials, method, uri string, statusCode int, request interface{}, response interface{}) (int, string, string) {
	var body io.Reader

	if request != nil {
		requestBody, err := json.Marshal(request)
		if err != nil {
			return -1, fmt.Sprintf("failed to marshal project object: %v", err), ""
		}
		body = bytes.NewReader(requestBody)
	}

	return n.doRaw(runner, uri, method, statusCode, body, "application/json", response, nil)
}

func (n *GitLabClient) GetBuild(config RunnerConfig) (*GetBuildResponse, bool) {
	request := GetBuildRequest{
		Info:  n.getRunnerVersion(config),
		Token: config.Token,
	}

	var response GetBuildResponse
	result, statusText, certificates := n.doJson(config.RunnerCredentials, "POST", "builds/register.json", 201, &request, &response)

	switch result {
	case 201:
		config.Log().Println("Checking for builds...", "received")
		response.TLSCAChain = certificates
		return &response, true
	case 403:
		config.Log().Errorln("Checking for builds...", "forbidden")
		return nil, false
	case 204, 404:
		config.Log().Debugln("Checking for builds...", "nothing")
		return nil, true
	case clientError:
		config.Log().WithField("status", statusText).Errorln("Checking for builds...", "error")
		return nil, false
	default:
		config.Log().WithField("status", statusText).Warningln("Checking for builds...", "failed")
		return nil, true
	}
}

func (n *GitLabClient) RegisterRunner(runner RunnerCredentials, description, tags string) *RegisterRunnerResponse {
	// TODO: pass executor
	request := RegisterRunnerRequest{
		Info:        n.getRunnerVersion(RunnerConfig{}),
		Token:       runner.Token,
		Description: description,
		Tags:        tags,
	}

	var response RegisterRunnerResponse
	result, statusText, _ := n.doJson(runner, "POST", "runners/register.json", 201, &request, &response)

	switch result {
	case 201:
		runner.Log().Println("Registering runner...", "succeeded")
		return &response
	case 403:
		runner.Log().Errorln("Registering runner...", "forbidden (check registration token)")
		return nil
	case clientError:
		runner.Log().WithField("status", statusText).Errorln("Registering runner...", "error")
		return nil
	default:
		runner.Log().WithField("status", statusText).Errorln("Registering runner...", "failed")
		return nil
	}
}

func (n *GitLabClient) DeleteRunner(runner RunnerCredentials) bool {
	request := DeleteRunnerRequest{
		Token: runner.Token,
	}

	result, statusText, _ := n.doJson(runner, "DELETE", "runners/delete", 200, &request, nil)

	switch result {
	case 200:
		runner.Log().Println("Deleting runner...", "succeeded")
		return true
	case 403:
		runner.Log().Errorln("Deleting runner...", "forbidden")
		return false
	case clientError:
		runner.Log().WithField("status", statusText).Errorln("Deleting runner...", "error")
		return false
	default:
		runner.Log().WithField("status", statusText).Errorln("Deleting runner...", "failed")
		return false
	}
}

func (n *GitLabClient) VerifyRunner(runner RunnerCredentials) bool {
	request := VerifyRunnerRequest{
		Token: runner.Token,
	}

	// HACK: we use non-existing build id to check if receive forbidden or not found
	result, statusText, _ := n.doJson(runner, "PUT", fmt.Sprintf("builds/%d", -1), 200, &request, nil)

	switch result {
	case 404:
		// this is expected due to fact that we ask for non-existing job
		runner.Log().Println("Veryfing runner...", "is alive")
		return true
	case 403:
		runner.Log().Errorln("Veryfing runner...", "is removed")
		return false
	case clientError:
		runner.Log().WithField("status", statusText).Errorln("Veryfing runner...", "error")
		return false
	default:
		runner.Log().WithField("status", statusText).Errorln("Veryfing runner...", "failed")
		return true
	}
}

func (n *GitLabClient) UpdateBuild(config RunnerConfig, id int, state BuildState, trace string) UpdateState {
	request := UpdateBuildRequest{
		Info:  n.getRunnerVersion(config),
		Token: config.Token,
		State: state,
		Trace: trace,
	}

	result, statusText, _ := n.doJson(config.RunnerCredentials, "PUT", fmt.Sprintf("builds/%d.json", id), 200, &request, nil)
	switch result {
	case 200:
		config.Log().Println(id, "Submitting build to coordinator...", "ok")
		return UpdateSucceeded
	case 404:
		config.Log().Warningln(id, "Submitting build to coordinator...", "aborted")
		return UpdateAbort
	case 403:
		config.Log().Errorln(id, "Submitting build to coordinator...", "forbidden")
		return UpdateAbort
	case clientError:
		config.Log().WithField("status", statusText).Errorln(id, "Submitting build to coordinator...", "error")
		return UpdateAbort
	default:
		config.Log().WithField("status", statusText).Warningln(id, "Submitting build to coordinator...", "failed")
		return UpdateFailed
	}
}

func (n *GitLabClient) createArtifactsForm(mpw *multipart.Writer, artifactsFile string) error {
	wr, err := mpw.CreateFormFile("file", filepath.Base(artifactsFile))
	if err != nil {
		return err
	}

	file, err := os.Open(artifactsFile)
	if err != nil {
		return err
	}
	defer file.Close()

	fi, err := file.Stat()
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return errors.New("Failed to upload directories")
	}

	_, err = io.Copy(wr, file)
	if err != nil {
		return err
	}

	return nil
}

func (n *GitLabClient) UploadArtifacts(config RunnerCredentials, id int, artifactsFile string) UploadState {
	pr, pw := io.Pipe()
	defer pw.Close()

	mpw := multipart.NewWriter(pw)

	go func() {
		defer mpw.Close()
		defer pr.Close()
		err := n.createArtifactsForm(mpw, artifactsFile)
		if err != nil {
			pr.CloseWithError(err)
		}
	}()

	headers := make(http.Header)
	headers.Set("BUILD-TOKEN", config.Token)
	result, statusText, _ := n.doRaw(config, "POST", fmt.Sprintf("builds/%d/artifacts.json", id), 200, pr, mpw.FormDataContentType(), nil, headers)

	switch result {
	case 200:
		logrus.Println(config.ShortDescription(), id, "Uploading artifacts to coordinator...", "ok")
		return UploadSucceeded
	case 403:
		logrus.Errorln(config.ShortDescription(), id, "Uploading artifacts to coordinator...", "forbidden")
		return UploadForbidden
	case 413:
		logrus.Errorln(config.ShortDescription(), id, "Uploading artifacts to coordinator...", "too large archive")
		return UploadTooLarge
	case clientError:
		logrus.Errorln(config.ShortDescription(), id, "Uploading artifacts to coordinator...", "error", statusText)
		return UploadFailed
	default:
		logrus.Warningln(config.ShortDescription(), id, "Uploading artifacts to coordinator...", "failed", statusText)
		return UploadFailed
	}
}