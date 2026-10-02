package container

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"k8s.io/klog/v2"
	kindexec "sigs.k8s.io/kind/pkg/exec"
)

// TODO we can do it as in KIND
var containerRuntime = "docker"

// ErrNotFound is returned by Inspect when the container does not exist.
var ErrNotFound = errors.New("container not found")

// Info is the subset of the container state used to reconcile it, read in one inspect call.
type Info struct {
	// Status is the runtime state: running, restarting, exited, created, paused, ...
	Status string
	IPv4   string
	IPv6   string
	// Ports maps the published container ports, in port/protocol format, to the host port.
	Ports map[string]string
}

// Running reports if the container is up. A restarting container is not running:
// the runtime is in control and reports no addresses or ports for it.
func (i *Info) Running() bool {
	return i.Status == "running"
}

// Inspect returns a consistent snapshot of the container state.
// It returns ErrNotFound if the container can not be inspected, the
// error message differs between runtimes.
func Inspect(name string) (*Info, error) {
	cmd := kindexec.Command(containerRuntime, "inspect", name)
	output, err := kindexec.Output(cmd)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrNotFound, name, err)
	}
	return parseInspect(output)
}

func parseInspect(data []byte) (*Info, error) {
	var containers []struct {
		State struct {
			Status string `json:"Status"`
		} `json:"State"`
		NetworkSettings struct {
			Ports    map[string][]portMapping `json:"Ports"`
			Networks map[string]struct {
				IPAddress         string `json:"IPAddress"`
				GlobalIPv6Address string `json:"GlobalIPv6Address"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := json.Unmarshal(data, &containers); err != nil {
		return nil, fmt.Errorf("failed to parse container details: %w", err)
	}
	if len(containers) != 1 {
		return nil, fmt.Errorf("expected 1 container, got %d", len(containers))
	}
	c := containers[0]
	info := &Info{
		Status: c.State.Status,
		Ports:  parsePortMappings(c.NetworkSettings.Ports),
	}
	for _, network := range c.NetworkSettings.Networks {
		if info.IPv4 == "" {
			info.IPv4 = network.IPAddress
		}
		if info.IPv6 == "" {
			info.IPv6 = network.GlobalIPv6Address
		}
	}
	return info, nil
}

// dockerIsAvailable checks if docker is available and the daemon is running
func dockerIsAvailable() bool {
	cmd := kindexec.Command("docker", "info")
	_, err := kindexec.OutputLines(cmd)
	return err == nil
}

func podmanIsAvailable() bool {
	cmd := kindexec.Command("podman", "info")
	_, err := kindexec.OutputLines(cmd)
	return err == nil
}

func nerdctlIsAvailable() bool {
	cmd := kindexec.Command("nerdctl", "info")
	if _, err := kindexec.OutputLines(cmd); err == nil {
		return true
	}
	cmd = kindexec.Command("finch", "info")
	_, err := kindexec.OutputLines(cmd)
	return err == nil
}

// Runtime returns the detected container runtime name.
func Runtime() string {
	return containerRuntime
}

// SetRuntime overrides the auto-detected container runtime.
func SetRuntime(name string) {
	containerRuntime = name
}

// DetectRootless returns true if rootless mode is detected, false otherwise
func DetectRootless() (bool, error) {
	var fstr = ""
	switch containerRuntime {
	case "docker":
		fstr = "{{range $opt := .SecurityOptions}}{{ if eq $opt \"name=rootless\" }}{{\"true\"}}{{end}}{{end}}"
	case "podman":
		fstr = "{{.Host.Security.Rootless}}"
	}
	if fstr == "" {
		return false, fmt.Errorf("unable to determine rootless for provider %s, assuming false", containerRuntime)
	}
	cmd := kindexec.Command(containerRuntime, "info", "-f", fstr)
	result, err := kindexec.OutputLines(cmd)
	if err != nil || len(result) == 0 {
		return false, err
	}
	return result[0] == "true", nil
}

// DetectRuntime probes the system for an available container runtime.
// It returns an error if no supported runtime is installed and running.
func DetectRuntime() (string, error) {
	if dockerIsAvailable() {
		return "docker", nil
	}
	if podmanIsAvailable() {
		return "podman", nil
	}
	if nerdctlIsAvailable() {
		if _, err := exec.LookPath("nerdctl"); err != nil {
			if _, err := exec.LookPath("finch"); err == nil {
				return "finch", nil
			}
		}
		return "nerdctl", nil
	}
	return "", fmt.Errorf("no supported container runtime found")
}

func Logs(name string, w io.Writer) error {
	cmd := exec.Command(containerRuntime, []string{"logs", name}...)
	cmd.Stderr = w
	cmd.Stdout = w
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("failed to get container logs: %w", err)
	}
	return nil
}

func LogDump(containerName string, fileName string) error {
	f, err := os.Create(fileName)
	if err != nil {
		return err
	}
	defer f.Close()

	err = Logs(containerName, f)
	if err != nil {
		return err
	}
	return nil
}

func Create(name string, args []string) error {
	if err := exec.Command(containerRuntime, append([]string{"run", "--name", name}, args...)...).Run(); err != nil {
		return err
	}
	return nil
}

func Restart(name string) error {
	if err := exec.Command(containerRuntime, []string{"restart", name}...).Run(); err != nil {
		return err
	}
	return nil
}

func Delete(name string) error {
	if err := exec.Command(containerRuntime, []string{"rm", "-f", name}...).Run(); err != nil {
		return err
	}
	return nil
}

func IsRunning(name string) bool {
	cmd := exec.Command(containerRuntime, []string{"ps", "-q", "-f", "name=" + name}...)
	output, err := cmd.Output()
	if err != nil || len(output) == 0 {
		return false
	}
	return true
}

func Exist(name string) bool {
	err := exec.Command(containerRuntime, []string{"inspect", name}...).Run()
	return err == nil
}

func Signal(name string, signal string) error {
	err := exec.Command(containerRuntime, []string{"kill", "-s", signal, name}...).Run()
	return err
}

func Exec(name string, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	args := []string{"exec", "--privileged"}
	if stdin != nil {
		args = append(args, "-i")
	}
	args = append(args, name)
	args = append(args, command...)
	cmd := exec.Command(containerRuntime, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	if stdout != nil {
		cmd.Stdout = stdout
	}
	if stderr != nil {
		cmd.Stderr = stderr
	}
	return cmd.Run()
}

func IPs(name string) (ipv4 string, ipv6 string, err error) {
	// retrieve the IP address of the node using docker inspect
	cmd := kindexec.Command(containerRuntime, "inspect",
		"-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}},{{.GlobalIPv6Address}}{{end}}",
		name, // ... against the "node" container
	)
	lines, err := kindexec.OutputLines(cmd)
	if err != nil {
		return "", "", fmt.Errorf("failed to get container details: %w", err)
	}
	if len(lines) != 1 {
		return "", "", fmt.Errorf("file should only be one line, got %d lines: %w", len(lines), err)
	}
	ips := strings.Split(lines[0], ",")
	if len(ips) != 2 {
		return "", "", fmt.Errorf("container addresses should have 2 values, got %d values", len(ips))
	}
	return ips[0], ips[1], nil
}

// return a list with the map of the internal port to the external port
func PortMaps(name string) (map[string]string, error) {
	// retrieve the IP address of the node using docker inspect
	cmd := kindexec.Command(containerRuntime, "inspect",
		"-f", "{{ json .NetworkSettings.Ports }}",
		name, // ... against the "node" container
	)

	lines, err := kindexec.OutputLines(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to get container details: %w", err)
	}
	if len(lines) != 1 {
		return nil, fmt.Errorf("file should only be one line, got %d lines: %w", len(lines), err)
	}

	portMappings := make(map[string][]portMapping)
	err = json.Unmarshal([]byte(lines[0]), &portMappings)
	if err != nil {
		return nil, err
	}
	return parsePortMappings(portMappings), nil
}

type portMapping struct {
	HostPort string `json:"HostPort"`
	HostIP   string `json:"HostIp"`
}

// parsePortMappings returns the map of the published TCP and UDP container ports,
// in port/protocol format, to the host port.
func parsePortMappings(portMappings map[string][]portMapping) map[string]string {
	result := map[string]string{}
	for k, v := range portMappings {
		protocol := "tcp"
		parts := strings.Split(k, "/")
		if len(parts) == 2 {
			protocol = strings.ToLower(parts[1])
		}
		if protocol != "tcp" && protocol != "udp" {
			klog.Infof("skipping protocol %s not supported, only UDP and TCP", protocol)
			continue
		}

		// TODO we just can get the first entry or look for ip families
		for _, pm := range v {
			if pm.HostPort != "" {
				result[parts[0]+"/"+protocol] = pm.HostPort
				break
			}
		}
	}
	return result
}

func ListByLabel(label string) ([]string, error) {
	cmd := kindexec.Command(containerRuntime,
		"ps",
		"-a", // show stopped nodes
		// filter for nodes with the cluster label
		"--filter", "label="+label,
		// format to include the cluster name
		"--format", `{{.ID }}`,
	)
	lines, err := kindexec.OutputLines(cmd)
	return lines, err
}

// GetLabelValue return the value of the associated label
// It returns an error if the label value does not exist
func GetLabelValue(name string, label string) (string, error) {
	cmd := kindexec.Command(containerRuntime,
		"inspect",
		"--format", fmt.Sprintf(`{{ index .Config.Labels "%s"}}`, label),
		name,
	)
	lines, err := kindexec.OutputLines(cmd)
	if err != nil {
		return "", err
	}
	if len(lines) != 1 {
		return "", fmt.Errorf("expected 1 line, got %d", len(lines))
	}
	return lines[0], nil
}
