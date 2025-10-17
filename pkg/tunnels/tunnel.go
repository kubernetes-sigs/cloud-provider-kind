package tunnels

import (
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"sigs.k8s.io/cloud-provider-kind/pkg/container"
)

type TunnelManager struct {
	mu      sync.Mutex
	tunnels map[string]map[string]*tunnel // first key is the service namespace/name second key is the servicePort
}

func NewTunnelManager() *TunnelManager {
	t := &TunnelManager{
		tunnels: map[string]map[string]*tunnel{},
	}
	return t
}

func (t *TunnelManager) SetupTunnels(containerName string) error {
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
	output, err := AddIPToLocalInterface(ipv4)
	if err != nil {
		return fmt.Errorf("error adding IP to local interface: %w - %s", err, output)
	}

	// create tunnel from the ip:svcport to the localhost:portmap
	t.mu.Lock()
	defer t.mu.Unlock()
	// There is one IP per Service and a tunnel per Service Port
	for containerPort, hostPort := range portmaps {
		parts := strings.Split(containerPort, "/")
		if len(parts) != 2 {
			return fmt.Errorf("expected format port/protocol for container port, got %s", containerPort)
		}

		tun := NewTunnel(ipv4, parts[0], parts[1], "localhost", hostPort)
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

func (t *TunnelManager) RemoveTunnels(containerName string) error {
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

	klog.V(0).Infof("Removing IPv4 address %s associated to local interface", tunnelIP)
	output, err := RemoveIPFromLocalInterface(tunnelIP)
	if err != nil {
		return fmt.Errorf("error removing IP from local interface: %w - %s", err, output)
	}
	return nil
}

// tunnel listens on localIP:localPort and proxies the connection to remoteIP:remotePort
type tunnel struct {
	listener   net.Listener
	udpConn    *net.UDPConn
	udpDone    chan struct{}
	localIP    string
	localPort  string
	protocol   string
	remoteIP   string // address:Port
	remotePort string
}

func NewTunnel(localIP, localPort, protocol, remoteIP, remotePort string) *tunnel {
	return &tunnel{
		localIP:    localIP,
		localPort:  localPort,
		protocol:   protocol,
		remoteIP:   remoteIP,
		remotePort: remotePort,
	}
}

func (t *tunnel) Start() error {
	klog.Infof("Starting tunnel on %s %s", net.JoinHostPort(t.localIP, t.localPort), t.protocol)
	switch t.protocol {
	case "udp":
		localAddrStr := net.JoinHostPort(t.localIP, t.localPort)
		udpAddr, err := net.ResolveUDPAddr("udp4", localAddrStr)
		if err != nil {
			return err
		}
		conn, err := net.ListenUDP("udp4", udpAddr)
		if err != nil {
			return err
		}

		// use a channel for signaling that the UDP connection has been closed
		t.udpDone = make(chan struct{})
		t.udpConn = conn

		go func() {
			for {
				select {
				case <-t.udpDone:
					return
				default:
				}

				err = t.handleUDPConnection(conn)
				if err != nil {
					klog.Infof("unexpected error on connection: %v", err)
				}
			}
		}()
	case "tcp", "tcp4", "tcp6":
		ln, err := net.Listen(t.protocol, net.JoinHostPort(t.localIP, t.localPort))
		if err != nil {
			return err
		}

		t.listener = ln

		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					klog.Infof("unexpected error listening: %v", err)
					return
				}
				tcpConn, ok := conn.(*net.TCPConn)
				if !ok {
					klog.Errorf("unexpected connection type %T, expected *net.TCPConn", conn)
					conn.Close() // nolint: errcheck
					continue
				}
				go func() {
					err := t.handleTCPConnection(tcpConn)
					if err != nil {
						klog.Infof("unexpected error on connection: %v", err)
					}
				}()
			}
		}()
	default:
		return fmt.Errorf("unsupported protocol %s", t.protocol)
	}
	return nil
}

func (t *tunnel) Stop() error {
	if t.listener != nil {
		_ = t.listener.Close()
	}
	if t.udpConn != nil {
		// Let the UDP handling code know that it can give up
		select {
		case <-t.udpDone:
		default:
			close(t.udpDone)
		}
		_ = t.udpConn.Close()
	}
	return nil
}

func (t *tunnel) handleTCPConnection(local *net.TCPConn) error {
	tcpAddr, err := net.ResolveTCPAddr(t.protocol, net.JoinHostPort(t.remoteIP, t.remotePort))
	if err != nil {
		return fmt.Errorf("can't resolve remote address %q: %v", net.JoinHostPort(t.remoteIP, t.remotePort), err)
	}
	remote, err := net.DialTCP(t.protocol, nil, tcpAddr)
	if err != nil {
		return fmt.Errorf("can't connect to server %q: %v", net.JoinHostPort(t.remoteIP, t.remotePort), err)
	}

	// Fully close both connections on return.
	defer remote.Close()
	defer local.Close()

	wg := &sync.WaitGroup{}
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(local, remote) // nolint: errcheck
		// Half-close the remote to local path.
		local.CloseWrite() // nolint: errcheck
		remote.CloseRead() // nolint: errcheck
	}()
	go func() {
		defer wg.Done()
		io.Copy(remote, local) // nolint: errcheck
		// Half-close the local to remote path.
		remote.CloseWrite() // nolint: errcheck
		local.CloseRead()   // nolint: errcheck
	}()

	wg.Wait()
	return nil
}

func (t *tunnel) handleUDPConnection(conn *net.UDPConn) error {
	buf := make([]byte, 1500)
	nRead, srcAddr, err := conn.ReadFromUDP(buf)
	if err != nil {
		return err
	}
	klog.V(4).Infof("Read %d bytes from %s", nRead, srcAddr.String())

	klog.V(4).Infof("Connecting to %s", net.JoinHostPort(t.remoteIP, t.remotePort))
	remoteConn, err := net.Dial("udp4", net.JoinHostPort(t.remoteIP, t.remotePort))
	if err != nil {
		return fmt.Errorf("can't connect to server %q: %v", net.JoinHostPort(t.remoteIP, t.remotePort), err)
	}
	defer remoteConn.Close()

	nWrite, err := remoteConn.Write(buf[:nRead])
	if err != nil {
		return fmt.Errorf("fail to write to remote %s: %s", remoteConn.RemoteAddr(), err)
	} else if nWrite < nRead {
		klog.V(2).Infof("Buffer underflow %d < %d to remote %s", nWrite, nRead, remoteConn.RemoteAddr())
	}
	klog.V(4).Infof("Wrote %d bytes to to %s", nWrite, remoteConn.RemoteAddr().String())

	buf = make([]byte, 1500)
	err = remoteConn.SetReadDeadline(time.Now().Add(5 * time.Second)) // Add deadline to ensure it doesn't block forever
	if err != nil {
		return fmt.Errorf("can not set read deadline: %v", err)
	}
	nRead, err = remoteConn.Read(buf)
	if err != nil {
		return fmt.Errorf("fail to read from remote %s: %s", remoteConn.RemoteAddr(), err)
	}
	klog.V(4).Infof("Read %d bytes from %s", nRead, remoteConn.RemoteAddr().String())

	_, err = conn.WriteToUDP(buf[:nRead], srcAddr)

	return err
}
