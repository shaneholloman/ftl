package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/yarlson/ftl/pkg/console"

	"github.com/yarlson/ftl/pkg/config"
	"github.com/yarlson/ftl/pkg/proxy"
)

const (
	newContainerSuffix = "_new"
)

type Executor interface {
	RunCommand(ctx context.Context, command string, args ...string) (io.Reader, error)
	CopyFile(ctx context.Context, from, to string) error
}

type Deployment struct {
	executor Executor
}

func NewDeployment(executor Executor) *Deployment {
	return &Deployment{executor: executor}
}

func (d *Deployment) Deploy(project string, cfg *config.Config) error {
	if err := console.ProgressSpinner(context.Background(), "Creating network", "Network created", []func() error{
		func() error { return d.createNetwork(project) },
	}); err != nil {
		return fmt.Errorf("failed to create network: %w", err)
	}

	if err := console.ProgressSpinner(context.Background(), "Creating volumes", "Volumes created", []func() error{
		func() error {
			for _, volume := range cfg.Volumes {
				if err := d.createVolume(project, volume); err != nil {
					return fmt.Errorf("failed to create volume: %w", err)
				}
			}
			return nil
		},
	}); err != nil {
		return fmt.Errorf("failed to create volumes: %w", err)
	}

	for _, dependency := range cfg.Dependencies {
		if err := console.ProgressSpinner(context.Background(),
			fmt.Sprintf("Creating dependency %s", dependency.Name),
			fmt.Sprintf("Dependency %s created", dependency.Name),
			[]func() error{
				func() error { return d.startDependency(project, &dependency) },
			}); err != nil {
			return fmt.Errorf("failed to create dependency %s: %w", dependency.Name, err)
		}
	}

	for _, service := range cfg.Services {
		if err := console.ProgressSpinner(context.Background(),
			fmt.Sprintf("Deploying service: %s", service.Name),
			fmt.Sprintf("Service deployed: %s", service.Name),
			[]func() error{
				func() error { return d.deployService(project, &service) },
			}); err != nil {
			return fmt.Errorf("failed to deploy service %s: %w", service.Name, err)
		}
	}

	if err := console.ProgressSpinner(context.Background(), "Starting proxy", "Proxy started", []func() error{
		func() error { return d.startProxy(cfg.Project.Name, cfg) },
	}); err != nil {
		return fmt.Errorf("failed to start proxy: %w", err)
	}

	return nil
}

func (d *Deployment) startProxy(project string, cfg *config.Config) error {
	projectPath, err := d.prepareProjectFolder(project)
	if err != nil {
		return fmt.Errorf("failed to prepare project folder: %w", err)
	}

	configPath, err := d.prepareNginxConfig(cfg, projectPath)
	if err != nil {
		return fmt.Errorf("failed to prepare nginx config: %w", err)
	}

	service := &config.Service{
		Name:  "proxy",
		Image: "yarlson/zero-nginx:latest",
		Port:  80,
		Volumes: []string{
			projectPath + "/:/etc/nginx/ssl",
			configPath + ":/etc/nginx/conf.d",
		},
		EnvVars: map[string]string{
			"DOMAIN": cfg.Project.Domain,
			"EMAIL":  cfg.Project.Email,
		},
		Forwards: []string{
			"80:80",
			"443:443",
		},
		HealthCheck: &config.HealthCheck{
			Path:     "/",
			Interval: time.Second,
			Timeout:  time.Second,
			Retries:  30,
		},
	}

	if err := d.deployService(project, service); err != nil {
		return fmt.Errorf("failed to deploy service %s: %w", service.Name, err)
	}

	if err := d.reloadNginxConfig(context.Background()); err != nil {
		return fmt.Errorf("failed to reload nginx config: %w", err)
	}

	return nil
}

func (d *Deployment) startDependency(project string, dependency *config.Dependency) error {
	if _, err := d.pullImage(dependency.Image); err != nil {
		return fmt.Errorf("failed to pull image for %s: %v", dependency.Image, err)
	}

	service := &config.Service{
		Name:    dependency.Name,
		Image:   dependency.Image,
		Volumes: dependency.Volumes,
		EnvVars: dependency.EnvVars,
	}
	if err := d.deployService(project, service); err != nil {
		return fmt.Errorf("failed to start container for %s: %v", dependency.Image, err)
	}

	return nil
}

func (d *Deployment) installService(project string, service *config.Service) error {
	if _, err := d.pullImage(service.Image); err != nil {
		return fmt.Errorf("failed to pull image for %s: %v", service.Image, err)
	}

	if err := d.startContainer(project, service, ""); err != nil {
		return fmt.Errorf("failed to start container for %s: %v", service.Image, err)
	}

	svcName := service.Name

	if err := d.performHealthChecks(svcName, service.HealthCheck); err != nil {
		return fmt.Errorf("install failed for %s: container is unhealthy: %w", svcName, err)
	}

	return nil
}

func (d *Deployment) updateService(project string, service *config.Service) error {
	svcName := service.Name

	if _, err := d.pullImage(service.Image); err != nil {
		return fmt.Errorf("failed to pull new image for %s: %v", svcName, err)
	}

	if err := d.startContainer(project, service, newContainerSuffix); err != nil {
		return fmt.Errorf("failed to start new container for %s: %v", svcName, err)
	}

	if err := d.performHealthChecks(svcName+newContainerSuffix, service.HealthCheck); err != nil {
		if _, err := d.runCommand(context.Background(), "docker", "rm", "-f", svcName+newContainerSuffix); err != nil {
			return fmt.Errorf("update failed for %s: new container is unhealthy and cleanup failed: %v", svcName, err)
		}
		return fmt.Errorf("update failed for %s: new container is unhealthy: %w", svcName, err)
	}

	oldContID, err := d.switchTraffic(project, svcName)
	if err != nil {
		return fmt.Errorf("failed to switch traffic for %s: %v", svcName, err)
	}

	if err := d.cleanup(oldContID, svcName); err != nil {
		return fmt.Errorf("failed to cleanup for %s: %v", svcName, err)
	}

	return nil
}

type containerInfo struct {
	ID     string
	Config struct {
		Image  string
		Env    []string
		Labels map[string]string
	}
	Image           string
	NetworkSettings struct {
		Networks map[string]struct{ Aliases []string }
	}
	HostConfig struct {
		Binds []string
	}
}

func (d *Deployment) getContainerID(project, service string) (string, error) {
	info, err := d.getContainerInfo(service, project)
	if err != nil {
		return "", err
	}

	return info.ID, err
}

func (d *Deployment) getContainerInfo(service, network string) (*containerInfo, error) {
	output, err := d.runCommand(context.Background(), "docker", "ps", "-aq", "--filter", fmt.Sprintf("network=%s", network))
	if err != nil {
		return nil, fmt.Errorf("failed to get container IDs: %w", err)
	}

	containerIDs := strings.Fields(output)
	for _, cid := range containerIDs {
		inspectOutput, err := d.runCommand(context.Background(), "docker", "inspect", cid)
		if err != nil {
			continue
		}

		var containerInfos []containerInfo
		if err := json.Unmarshal([]byte(inspectOutput), &containerInfos); err != nil || len(containerInfos) == 0 {
			continue
		}

		if aliases, ok := containerInfos[0].NetworkSettings.Networks[network]; ok {
			for _, alias := range aliases.Aliases {
				if alias == service {
					return &containerInfos[0], nil
				}
			}
		}
	}

	return nil, fmt.Errorf("no container found with alias %s in network %s", service, network)
}

func (d *Deployment) startContainer(project string, service *config.Service, suffix string) error {
	svcName := service.Name

	args := []string{"run", "-d", "--name", svcName + suffix, "--network", project, "--network-alias", svcName + suffix}

	for key, value := range service.EnvVars {
		args = append(args, "-e", fmt.Sprintf("%s=%s", key, value))
	}

	for _, volume := range service.Volumes {
		if unicode.IsLetter(rune(volume[0])) {
			volume = fmt.Sprintf("%s-%s", project, volume)
		}
		args = append(args, "-v", volume)
	}

	if service.HealthCheck != nil {
		args = append(args, "--health-cmd", fmt.Sprintf("curl -sf http://localhost:%d%s || exit 1", service.Port, service.HealthCheck.Path))
		args = append(args, "--health-interval", fmt.Sprintf("%ds", int(service.HealthCheck.Interval.Seconds())))
		args = append(args, "--health-retries", fmt.Sprintf("%d", service.HealthCheck.Retries))
		args = append(args, "--health-timeout", fmt.Sprintf("%ds", int(service.HealthCheck.Timeout.Seconds())))
	}

	if len(service.Forwards) > 0 {
		for _, forward := range service.Forwards {
			args = append(args, "-p", forward)
		}
	}

	hash, err := service.Hash()
	if err != nil {
		return fmt.Errorf("failed to generate config hash: %w", err)
	}
	args = append(args, "--label", fmt.Sprintf("ftl.config-hash=%s", hash))
	args = append(args, service.Image)

	_, err = d.runCommand(context.Background(), "docker", args...)
	return err
}

func (d *Deployment) performHealthChecks(container string, healthCheck *config.HealthCheck) error {
	if healthCheck == nil {
		return nil
	}

	for i := 0; i < healthCheck.Retries; i++ {
		output, err := d.runCommand(context.Background(), "docker", "inspect", "--format={{.State.Health.Status}}", container)
		if err == nil && strings.TrimSpace(output) == "healthy" {
			return nil
		}
		time.Sleep(healthCheck.Interval)
	}
	return fmt.Errorf("container failed to become healthy")
}

func (d *Deployment) switchTraffic(project, service string) (string, error) {
	newContainer := service + newContainerSuffix
	oldContainer, err := d.getContainerID(project, service)
	if err != nil {
		return "", fmt.Errorf("failed to get old container ID: %v", err)
	}

	cmds := [][]string{
		{"docker", "network", "disconnect", project, newContainer},
		{"docker", "network", "connect", "--alias", service, project, newContainer},
	}

	for _, cmd := range cmds {
		if _, err := d.runCommand(context.Background(), cmd[0], cmd[1:]...); err != nil {
			return "", fmt.Errorf("failed to execute command '%s': %v", strings.Join(cmd, " "), err)
		}
	}

	time.Sleep(1 * time.Second)

	cmds = [][]string{
		{"docker", "network", "disconnect", project, oldContainer},
	}

	for _, cmd := range cmds {
		if _, err := d.runCommand(context.Background(), cmd[0], cmd[1:]...); err != nil {
			return "", fmt.Errorf("failed to execute command '%s': %v", strings.Join(cmd, " "), err)
		}
	}

	return oldContainer, nil
}

func (d *Deployment) cleanup(oldContID, service string) error {
	cmds := [][]string{
		{"docker", "stop", oldContID},
		{"docker", "rm", oldContID},
		{"docker", "rename", service + newContainerSuffix, service},
	}

	for _, cmd := range cmds {
		if _, err := d.runCommand(context.Background(), cmd[0], cmd[1:]...); err != nil {
			return fmt.Errorf("failed to execute command '%s': %v", strings.Join(cmd, " "), err)
		}
	}

	return nil
}

func (d *Deployment) pullImage(imageName string) (string, error) {
	_, err := d.runCommand(context.Background(), "docker", "pull", imageName)
	if err != nil {
		return "", err
	}

	output, err := d.runCommand(context.Background(), "docker", "images", "--no-trunc", "--format={{.ID}}", imageName)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(output), nil
}

func (d *Deployment) runCommand(ctx context.Context, command string, args ...string) (string, error) {
	output, err := d.executor.RunCommand(ctx, command, args...)
	if err != nil {
		return "", fmt.Errorf("failed to run command: %w", err)
	}

	outputBytes, readErr := io.ReadAll(output)
	if readErr != nil {
		return "", fmt.Errorf("failed to read command output: %v (original error: %w)", readErr, err)
	}

	return strings.TrimSpace(string(outputBytes)), nil
}

func (d *Deployment) makeProjectFolder(projectName string) error {
	projectPath, err := d.projectFolder(projectName)
	if err != nil {
		return fmt.Errorf("failed to get project folder path: %w", err)
	}

	_, err = d.runCommand(context.Background(), "mkdir", "-p", projectPath)
	return err
}

func (d *Deployment) projectFolder(projectName string) (string, error) {
	homeDir, err := d.runCommand(context.Background(), "sh", "-c", "echo $HOME")
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	homeDir = strings.TrimSpace(homeDir)
	projectPath := filepath.Join(homeDir, "projects", projectName)

	return projectPath, nil
}

func (d *Deployment) prepareProjectFolder(project string) (string, error) {
	if err := d.makeProjectFolder(project); err != nil {
		return "", fmt.Errorf("failed to create project folder: %w", err)
	}

	return d.projectFolder(project)
}

func (d *Deployment) prepareNginxConfig(cfg *config.Config, projectPath string) (string, error) {
	nginxConfig, err := proxy.GenerateNginxConfig(cfg)
	if err != nil {
		return "", fmt.Errorf("failed to generate nginx config: %w", err)
	}

	nginxConfig = strings.TrimSpace(nginxConfig)

	configPath := filepath.Join(projectPath, "nginx")
	_, err = d.runCommand(context.Background(), "mkdir", "-p", configPath)
	if err != nil {
		return "", fmt.Errorf("failed to create nginx config directory: %w", err)
	}

	tmpFile, err := os.CreateTemp("", "nginx-config-*.conf")
	if err != nil {
		return "", fmt.Errorf("failed to create temporary file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(nginxConfig); err != nil {
		return "", fmt.Errorf("failed to write nginx config to temporary file: %w", err)
	}

	return configPath, d.executor.CopyFile(context.Background(), tmpFile.Name(), filepath.Join(configPath, "default.conf"))
}

func (d *Deployment) serviceChanged(project string, service *config.Service) (bool, error) {
	containerInfo, err := d.getContainerInfo(service.Name, project)
	if err != nil {
		return false, fmt.Errorf("failed to get container info: %w", err)
	}

	hash, err := service.Hash()
	if err != nil {
		return false, fmt.Errorf("failed to generate config hash: %w", err)
	}

	return containerInfo.Config.Labels["ftl.config-hash"] != hash, nil
}

func (d *Deployment) deployService(project string, service *config.Service) error {
	hash, err := d.pullImage(service.Image)
	if err != nil {
		return fmt.Errorf("failed to pull image for %s: %w", service.Name, err)
	}

	containerInfo, err := d.getContainerInfo(service.Name, project)
	if err != nil {
		if err := d.installService(project, service); err != nil {
			return fmt.Errorf("failed to install service %s: %w", service.Name, err)
		}

		return nil
	}

	if hash != containerInfo.Image {
		if err := d.updateService(project, service); err != nil {
			return fmt.Errorf("failed to update service %s due to image change: %w", service.Name, err)
		}

		return nil
	}

	changed, err := d.serviceChanged(project, service)
	if err != nil {
		return fmt.Errorf("failed to check if service %s has changed: %w", service.Name, err)
	}

	if changed {
		if err := d.updateService(project, service); err != nil {
			return fmt.Errorf("failed to update service %s due to config change: %w", service.Name, err)
		}
	}

	return nil
}

func (d *Deployment) networkExists(network string) (bool, error) {
	output, err := d.runCommand(context.Background(), "docker", "network", "ls", "--format", "{{.Name}}")
	if err != nil {
		return false, fmt.Errorf("failed to list Docker networks: %w", err)
	}

	networks := strings.Split(strings.TrimSpace(output), "\n")
	for _, n := range networks {
		if strings.TrimSpace(n) == network {
			return true, nil
		}
	}
	return false, nil
}

func (d *Deployment) createNetwork(network string) error {
	exists, err := d.networkExists(network)
	if err != nil {
		return fmt.Errorf("failed to check if network exists: %w", err)
	}

	if exists {
		return nil
	}

	_, err = d.runCommand(context.Background(), "docker", "network", "create", network)
	if err != nil {
		return fmt.Errorf("failed to create network: %w", err)
	}

	return nil
}

func (d *Deployment) createVolume(project, volume string) error {
	volumeName := fmt.Sprintf("%s-%s", project, volume)
	if _, err := d.runCommand(context.Background(), "docker", "volume", "inspect", volumeName); err == nil {
		return nil
	}

	_, err := d.runCommand(context.Background(), "docker", "volume", "create", volumeName)
	if err != nil {
		return fmt.Errorf("failed to create volume: %w", err)
	}

	return nil
}

func (d *Deployment) CopyDockerImage(ctx context.Context, remoteHost, remoteUser, imageName string) error {
	localStore := filepath.Join(os.Getenv("HOME"), "docker-images")
	remoteStore, err := d.getRemoteDockerImageStore(ctx, remoteHost, remoteUser)
	if err != nil {
		return fmt.Errorf("failed to get remote docker image store: %w", err)
	}

	if needsSync, err := d.imageNeedsSync(ctx, remoteHost, remoteUser, imageName); err != nil {
		return fmt.Errorf("failed to check if image needs sync: %w", err)
	} else if !needsSync {
		fmt.Println("Images are identical on both hosts. Skipping sync.")
		return nil
	}

	imageDir := strings.ReplaceAll(strings.ReplaceAll(imageName, ":", "_"), "/", "_")
	localPath := filepath.Join(localStore, imageDir)
	remotePath := filepath.Join(remoteStore, imageDir)

	if err := d.saveAndExtractImage(ctx, imageName, localPath); err != nil {
		return fmt.Errorf("failed to save and extract image: %w", err)
	}

	if err := d.createRemoteDirectory(ctx, remoteHost, remoteUser, remotePath); err != nil {
		return fmt.Errorf("failed to create remote directory: %w", err)
	}

	if err := d.transferImageMetadata(ctx, remoteHost, remoteUser, localPath, remotePath); err != nil {
		return fmt.Errorf("failed to transfer image metadata: %w", err)
	}

	if err := d.transferImageBlobs(ctx, remoteHost, remoteUser, localPath, remotePath); err != nil {
		return fmt.Errorf("failed to transfer image blobs: %w", err)
	}

	if err := d.cleanupRemoteBlobs(ctx, remoteHost, remoteUser, localPath, remotePath); err != nil {
		return fmt.Errorf("failed to cleanup remote blobs: %w", err)
	}

	if err := d.loadImageOnRemoteHost(ctx, remoteHost, remoteUser, remotePath); err != nil {
		return fmt.Errorf("failed to load image on remote host: %w", err)
	}

	fmt.Println("Image sync completed successfully!")
	return nil
}

func (d *Deployment) getRemoteDockerImageStore(ctx context.Context, remoteHost, remoteUser string) (string, error) {
	cmd := "echo $HOME/docker-images"
	output, err := d.runRemoteCommand(ctx, remoteHost, remoteUser, cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

func (d *Deployment) imageNeedsSync(ctx context.Context, remoteHost, remoteUser, imageName string) (bool, error) {
	localInspect, err := d.runCommand(ctx, "docker", "inspect", imageName)
	if err != nil {
		return false, fmt.Errorf("failed to inspect local image: %w", err)
	}

	remoteInspect, err := d.runRemoteCommand(ctx, remoteHost, remoteUser, fmt.Sprintf("docker inspect %s", imageName))
	if err != nil {
		return true, nil
	}

	var localData, remoteData []map[string]interface{}
	if err := json.Unmarshal([]byte(localInspect), &localData); err != nil {
		return false, fmt.Errorf("failed to unmarshal local inspect data: %w", err)
	}
	if err := json.Unmarshal([]byte(remoteInspect), &remoteData); err != nil {
		return false, fmt.Errorf("failed to unmarshal remote inspect data: %w", err)
	}

	if len(localData) == 0 || len(remoteData) == 0 {
		return true, nil
	}

	localConfig := localData[0]["Config"].(map[string]interface{})
	remoteConfig := remoteData[0]["Config"].(map[string]interface{})
	delete(localConfig, "Image")
	delete(remoteConfig, "Image")

	return !jsonEqual(localConfig, remoteConfig) || !jsonEqual(localData[0]["RootFS"], remoteData[0]["RootFS"]), nil
}

func jsonEqual(a, b interface{}) bool {
	ja, err := json.Marshal(a)
	if err != nil {
		return false
	}
	jb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return string(ja) == string(jb)
}

func (d *Deployment) saveAndExtractImage(ctx context.Context, imageName, localPath string) error {
	if err := os.MkdirAll(localPath, 0755); err != nil {
		return fmt.Errorf("failed to create local directory: %w", err)
	}

	tarPath := filepath.Join(localPath, "image.tar")
	if _, err := d.runCommand(ctx, "docker", "save", imageName, "-o", tarPath); err != nil {
		return fmt.Errorf("failed to save docker image: %w", err)
	}

	if _, err := d.runCommand(ctx, "tar", "xf", tarPath, "-C", localPath); err != nil {
		return fmt.Errorf("failed to extract image: %w", err)
	}

	return os.Remove(tarPath)
}

func (d *Deployment) createRemoteDirectory(ctx context.Context, remoteHost, remoteUser, remotePath string) error {
	cmd := fmt.Sprintf("mkdir -p %s/blobs/sha256", remotePath)
	_, err := d.runRemoteCommand(ctx, remoteHost, remoteUser, cmd)
	return err
}

func (d *Deployment) transferImageMetadata(ctx context.Context, remoteHost, remoteUser, localPath, remotePath string) error {
	metadataFiles := []string{"index.json", "manifest.json", "oci-layout"}
	for _, file := range metadataFiles {
		localFile := filepath.Join(localPath, file)
		remoteFile := filepath.Join(remotePath, file)
		if err := d.copyFileToRemote(ctx, remoteHost, remoteUser, localFile, remoteFile); err != nil {
			return fmt.Errorf("failed to copy %s: %w", file, err)
		}
	}
	return nil
}

func (d *Deployment) transferImageBlobs(ctx context.Context, remoteHost, remoteUser, localPath, remotePath string) error {
	localBlobsDir := filepath.Join(localPath, "blobs", "sha256")
	remoteBlobsDir := filepath.Join(remotePath, "blobs", "sha256")

	localBlobs, err := d.listFiles(localBlobsDir)
	if err != nil {
		return fmt.Errorf("failed to list local blobs: %w", err)
	}

	remoteBlobs, err := d.listRemoteFiles(ctx, remoteHost, remoteUser, remoteBlobsDir)
	if err != nil {
		return fmt.Errorf("failed to list remote blobs: %w", err)
	}

	for _, blob := range localBlobs {
		if !contains(remoteBlobs, blob) {
			localFile := filepath.Join(localBlobsDir, blob)
			remoteFile := filepath.Join(remoteBlobsDir, blob)
			if err := d.copyFileToRemote(ctx, remoteHost, remoteUser, localFile, remoteFile); err != nil {
				return fmt.Errorf("failed to copy blob %s: %w", blob, err)
			}
		}
	}

	return nil
}

func (d *Deployment) cleanupRemoteBlobs(ctx context.Context, remoteHost, remoteUser, localPath, remotePath string) error {
	localBlobsDir := filepath.Join(localPath, "blobs", "sha256")
	remoteBlobsDir := filepath.Join(remotePath, "blobs", "sha256")

	localBlobs, err := d.listFiles(localBlobsDir)
	if err != nil {
		return fmt.Errorf("failed to list local blobs: %w", err)
	}

	remoteBlobs, err := d.listRemoteFiles(ctx, remoteHost, remoteUser, remoteBlobsDir)
	if err != nil {
		return fmt.Errorf("failed to list remote blobs: %w", err)
	}

	for _, blob := range remoteBlobs {
		if !contains(localBlobs, blob) {
			cmd := fmt.Sprintf("rm -f %s", filepath.Join(remoteBlobsDir, blob))
			if _, err := d.runRemoteCommand(ctx, remoteHost, remoteUser, cmd); err != nil {
				return fmt.Errorf("failed to remove obsolete blob %s: %w", blob, err)
			}
		}
	}

	return nil
}

func (d *Deployment) loadImageOnRemoteHost(ctx context.Context, remoteHost, remoteUser, remotePath string) error {
	cmd := fmt.Sprintf("cd %s && tar -cf - . | docker load", remotePath)
	_, err := d.runRemoteCommand(ctx, remoteHost, remoteUser, cmd)
	return err
}

func (d *Deployment) listFiles(dir string) ([]string, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var fileNames []string
	for _, file := range files {
		if !file.IsDir() {
			fileNames = append(fileNames, file.Name())
		}
	}
	return fileNames, nil
}

func (d *Deployment) listRemoteFiles(ctx context.Context, remoteHost, remoteUser, dir string) ([]string, error) {
	cmd := fmt.Sprintf("ls -1 %s 2>/dev/null || true", dir)
	output, err := d.runRemoteCommand(ctx, remoteHost, remoteUser, cmd)
	if err != nil {
		return nil, err
	}
	return strings.Fields(output), nil
}

func (d *Deployment) copyFileToRemote(ctx context.Context, remoteHost, remoteUser, localFile, remoteFile string) error {
	return d.executor.CopyFile(ctx, localFile, fmt.Sprintf("%s@%s:%s", remoteUser, remoteHost, remoteFile))
}

func (d *Deployment) runRemoteCommand(ctx context.Context, remoteHost, remoteUser, command string) (string, error) {
	sshCommand := fmt.Sprintf("ssh -o Compression=no -o TCPKeepAlive=yes -o ServerAliveInterval=60 %s@%s %s", remoteUser, remoteHost, command)
	return d.runCommand(ctx, "bash", "-c", sshCommand)
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func (d *Deployment) reloadNginxConfig(ctx context.Context) error {
	_, err := d.runCommand(ctx, "docker", "exec", "proxy", "nginx", "-s", "reload")
	return err
}
