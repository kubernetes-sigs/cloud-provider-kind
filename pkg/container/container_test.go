package container

import (
	"reflect"
	"testing"
)

// Samples captured from `docker inspect` (29.7) and `podman inspect` (5.8),
// trimmed to the fields parseInspect reads.
func TestParseInspect(t *testing.T) {
	tests := []struct {
		name     string
		data     string
		expected *Info
	}{
		{
			name: "docker running",
			data: `[{"State":{"Status":"running","Running":true,"Restarting":false},
"NetworkSettings":{"Ports":{"10000/tcp":[{"HostIp":"0.0.0.0","HostPort":"32773"},{"HostIp":"::","HostPort":"32773"}],
"17070/tcp":[{"HostIp":"0.0.0.0","HostPort":"17070"}],"17071/udp":[{"HostIp":"0.0.0.0","HostPort":"32769"},{"HostIp":"::","HostPort":"32769"}],
"9999/sctp":[{"HostIp":"0.0.0.0","HostPort":"9999"}],"8080/tcp":null},
"Networks":{"kind":{"IPAddress":"192.168.8.11","GlobalIPv6Address":"fc00:f853:ccd:e793::b"}}}}]`,
			expected: &Info{
				Status: "running",
				IPv4:   "192.168.8.11",
				IPv6:   "fc00:f853:ccd:e793::b",
				Ports:  map[string]string{"10000/tcp": "32773", "17070/tcp": "17070", "17071/udp": "32769"},
			},
		},
		{
			// docker clears the network state while the restart policy is in progress
			name: "docker restarting",
			data: `[{"State":{"Status":"restarting","Running":true,"Restarting":true},
"NetworkSettings":{"Ports":{},"Networks":{"kind":{"IPAddress":"","GlobalIPv6Address":""}}}}]`,
			expected: &Info{Status: "restarting", Ports: map[string]string{}},
		},
		{
			name: "docker exited",
			data: `[{"State":{"Status":"exited"},
"NetworkSettings":{"Ports":{},"Networks":{"kind":{"IPAddress":"","GlobalIPv6Address":""}}}}]`,
			expected: &Info{Status: "exited", Ports: map[string]string{}},
		},
		{
			name: "podman running",
			data: `[{"State":{"Status":"running","Running":true,"Restarting":false},
"NetworkSettings":{"Ports":{"10000/tcp":[{"HostIp":"0.0.0.0","HostPort":"38685"}],"17070/tcp":[{"HostIp":"0.0.0.0","HostPort":"17070"}],"17071/udp":[{"HostIp":"0.0.0.0","HostPort":"46853"}]},
"Networks":{"cpk-probe-net":{"IPAddress":"10.89.0.2","GlobalIPv6Address":""}}}}]`,
			expected: &Info{
				Status: "running",
				IPv4:   "10.89.0.2",
				Ports:  map[string]string{"10000/tcp": "38685", "17070/tcp": "17070", "17071/udp": "46853"},
			},
		},
		{
			// podman keeps the port bindings of a stopped container
			name: "podman exited",
			data: `[{"State":{"Status":"exited"},
"NetworkSettings":{"Ports":{"10000/tcp":[{"HostIp":"0.0.0.0","HostPort":"37925"}],"17070/tcp":[{"HostIp":"0.0.0.0","HostPort":"17070"}]},
"Networks":{"pasta":{"IPAddress":"","GlobalIPv6Address":""}}}}]`,
			expected: &Info{Status: "exited", Ports: map[string]string{"10000/tcp": "37925", "17070/tcp": "17070"}},
		},
		{
			// rootless podman without a user defined network has no Networks key
			name:     "no networks",
			data:     `[{"State":{"Status":"running"},"NetworkSettings":{"Ports":{}}}]`,
			expected: &Info{Status: "running", Ports: map[string]string{}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual, err := parseInspect([]byte(test.data))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(actual, test.expected) {
				t.Errorf("expected %+v, got %+v", test.expected, actual)
			}
		})
	}
}

func TestParseInspectErrors(t *testing.T) {
	for name, data := range map[string]string{
		"empty array": `[]`,
		"two entries": `[{"State":{"Status":"running"}},{"State":{"Status":"running"}}]`,
		"not json":    `error: no such object`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseInspect([]byte(data)); err == nil {
				t.Errorf("expected an error")
			}
		})
	}
}
