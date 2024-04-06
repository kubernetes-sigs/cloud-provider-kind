package loadbalancer

import (
	"fmt"
	"io"
	"net"
	"sync"

	"k8s.io/klog/v2"

	"sigs.k8s.io/cloud-provider-kind/pkg/container"
)

const (
	ifaceName = "lo0"
)

type tunnelManager struct {
	mu      sync.Mutex
	tunnels map[string]map[string]*tunnel // first key is the service namespace/name second key is the servicePort
}

func NewTunnelManager() *tunnelManager {
	t := &tunnelManager{
		tunnels: map[string]map[string]*tunnel{},
	}
	return t
}

func (t *tunnelManager) setupTunnels(containerName string) error {
	// get the portmapping from the container and its internal IPs and forward them
	// 1. Create the fake IP on the tunnel interface
	// 2. Capture the traffic directed to that IP port and forward to the exposed port in the host
	portmaps, err := container.PortMaps(containerName)
	if err != nil {
		return err
	}
	klog.V(0).Infof("found port maps %v associated to container %s", portmaps, containerName)

	ipv4, _, err := container.IPs(containerName)
	if err != nil {
		return err
	}

	klog.V(0).Infof("setting IPv4 address %s associated to container %s", ipv4, containerName)
	err = AddIPToInterface(ifaceName, ipv4)
	if err != nil {
		return err
	}

	// create tunnel from the ip:svcport to the localhost:portmap
	t.mu.Lock()
	defer t.mu.Unlock()
	// There is one IP per Service and a tunnel per Service Port
	for containerPort, hostPort := range portmaps {
		tun := NewTunnel(ipv4, containerPort, "localhost", hostPort)
		// TODO check if we can leak tunnels
		err = tun.Start()
		if err != nil {
			return err
		}
		_, ok := t.tunnels[containerName]
		if !ok {
			t.tunnels[containerName] = map[string]*tunnel{}
		}
		t.tunnels[containerName][containerPort] = tun
	}
	return nil
}

func (t *tunnelManager) removeTunnels(containerName string) error {
	klog.V(0).Infof("stopping tunnels on containers %s", containerName)
	t.mu.Lock()
	defer t.mu.Unlock()
	tunnels, ok := t.tunnels[containerName]
	if !ok {
		return nil
	}

	// all tunnels in the same container share the same local IP on the host
	var tunnelIP string
	for _, tunnel := range tunnels {
		if tunnelIP == "" {
			tunnelIP = tunnel.localIP
		}
		tunnel.Stop() // nolint: errcheck
	}

	klog.V(0).Infof("Removing IPv4 address %s associated to interface %s", tunnelIP, ifaceName)
	err := RemoveIPToInterface(ifaceName, tunnelIP)
	if err != nil {
		return err
	}
	return nil
}

// tunnel listens on localIP:localPort and proxies the connection to remoteIP:remotePort
type tunnel struct {
	listener   net.Listener
	localIP    string
	localPort  string
	remoteIP   string // address:Port
	remotePort string
}

func NewTunnel(localIP, localPort, remoteIP, remotePort string) *tunnel {
	return &tunnel{
		localIP:    localIP,
		localPort:  localPort,
		remoteIP:   remoteIP,
		remotePort: remotePort,
	}
}

func (t *tunnel) Start() error {
	klog.Infof("Starting tunnel on %s", net.JoinHostPort(t.localIP, t.localPort))
	ln, err := net.Listen("tcp", net.JoinHostPort(t.localIP, t.localPort))
	if err != nil {
		return err
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				klog.Infof("unexpected error listening: %v", err)
			} else {
				go func() {
					err := t.handleConnection(conn)
					if err != nil {
						klog.Infof("unexpected error on connection: %v", err)
					}
				}()
			}
		}
	}()
	return nil
}

func (t *tunnel) Stop() error {
	if t.listener != nil {
		return t.listener.Close()
	}
	return nil
}

func (t *tunnel) handleConnection(local net.Conn) error {
	remote, err := net.Dial("tcp", net.JoinHostPort(t.remoteIP, t.remotePort))
	if err != nil {
		return fmt.Errorf("can't connect to server %q: %v", net.JoinHostPort(t.remoteIP, t.remotePort), err)
	}
	defer remote.Close()

	wg := &sync.WaitGroup{}
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(local, remote)
	}()
	go func() {
		defer wg.Done()
		io.Copy(remote, local)
	}()
	wg.Wait()
	return nil
}
