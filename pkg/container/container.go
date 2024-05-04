package container

import (
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"k8s.io/klog/v2"
	kindexec "sigs.k8s.io/kind/pkg/exec"
)

// TODO we can do it as in KIND
var containerRuntime = "docker"

// dockerIsAvailable checks if docker is available in the system
func dockerIsAvailable() bool {
	cmd := kindexec.Command("docker", "-v")
	lines, err := kindexec.OutputLines(cmd)
	if err != nil || len(lines) != 1 {
		return false
	}
	return strings.HasPrefix(lines[0], "Docker version")
}

func podmanIsAvailable() bool {
	cmd := kindexec.Command("podman", "-v")
	lines, err := kindexec.OutputLines(cmd)
	if err != nil || len(lines) != 1 {
		return false
	}
	return strings.HasPrefix(lines[0], "podman version")

}

func init() {
	if dockerIsAvailable() {
		return
	}
	if podmanIsAvailable() {
		containerRuntime = "podman"
	}
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

	type portMapping struct {
		HostPort string `json:"HostPort"`
		HostIP   string `json:"HostIp"`
	}

	portMappings := make(map[string][]portMapping)
	err = json.Unmarshal([]byte(lines[0]), &portMappings)
	if err != nil {
		return nil, err
	}

	result := map[string]string{}
	for k, v := range portMappings {
		protocol := "tcp"
		parts := strings.Split(k, "/")
		if len(parts) == 2 {
			protocol = strings.ToLower(parts[1])
		}
		if protocol != "tcp" {
			klog.Infof("skipping protocol %s not supported, only TCP", protocol)
			continue
		}

		// TODO we just can get the first entry or look for ip families
		for _, pm := range v {
			if pm.HostPort != "" {
				result[parts[0]] = pm.HostPort
				break
			}
		}
	}
	return result, nil
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
