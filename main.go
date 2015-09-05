package main

/*
#include <sys/socket.h>
#include <sys/ioctl.h>
#include <fcntl.h>
#include <linux/if.h>
#include <linux/if_tun.h>
#include <stdlib.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

void new_tun(int* fdp, char** namep) {
	struct ifreq *ifr = (struct ifreq*)malloc(sizeof(struct ifreq));
	memset(ifr, 0, sizeof(struct ifreq));
	int fd;
	if ((fd = open("/dev/net/tun", O_RDWR)) < 0) return;
	ifr->ifr_flags = IFF_TUN;
	if (ioctl(fd, TUNSETIFF, (void*)ifr) < 0) close(fd);
	*fdp = fd;
	*namep = ifr->ifr_name;
	return;
}

*/
import "C"

import (
	crand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	socks "github.com/reusee/socks5-server"
)

const (
	MTU   = 1500
	PORTS = 8
)

var (
	sp = fmt.Sprintf
)

func init() {
	var seed int64
	binary.Read(crand.Reader, binary.LittleEndian, &seed)
	rand.Seed(seed)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Printf("usage: %s [config file path]", os.Args[0])
		return
	}

	// get config
	var config struct {
		RemoteAddr  string
		LocalAddr   string
		LocalPorts  []int
		RemotePorts []int
		Socks5Port  int
	}
	configContent, err := ioutil.ReadFile(os.Args[1])
	ce(err, "read config file")
	err = json.Unmarshal(configContent, &config)
	ce(err, "parse config file")
	if len(config.LocalPorts) != PORTS {
		ce(nil, sp("must be %d local ports", PORTS))
	}
	if len(config.RemotePorts) != 0 || len(config.RemotePorts) != PORTS {
		ce(nil, sp("must be zero or %d remote ports", PORTS))
	}
	if config.LocalAddr == "" {
		config.LocalAddr = "127.0.0.1"
	}
	if config.Socks5Port == 0 {
		config.Socks5Port = 1080
	}
	isLocal := len(config.RemotePorts) > 0

	// setup tun device
	var fd C.int
	var cName *C.char
	C.new_tun(&fd, &cName)
	if fd == 0 {
		ce(nil, "allocate tun")
	}
	name := C.GoString(cName)
	info("fd %d dev %s", fd, name)
	run("ip", "link", "set", name, "up")
	ip := "192.168.168.1"
	if isLocal {
		ip = "192.168.168.2"
	}
	run("ip", "addr", "add", ip+"/24", "dev", name)
	run("ip", "link", "set", "dev", name, "mtu", strconv.Itoa(MTU))
	run("ip", "link", "set", "dev", name, "qlen", "1000")
	file := os.NewFile(uintptr(fd), name)

	// start socks server
	if !isLocal {
		go startSocksServer(sp("%s:%d", ip, config.Socks5Port))
	}

	// setup remote addrs
	var remoteAddrs []*net.UDPAddr
	var remoteAddrsLock sync.Mutex
	for _, port := range config.RemotePorts {
		addr, err := net.ResolveUDPAddr("udp", sp("%s:%d", config.RemoteAddr, port))
		ce(err, "resolve addr")
		remoteAddrs = append(remoteAddrs, addr)
	}

	// listen local ports
	var localConns []*net.UDPConn
	for _, port := range config.LocalPorts {
		addr, err := net.ResolveUDPAddr("udp", sp("%s:%d", config.LocalAddr, port))
		ce(err, "resolve addr")
		conn, err := net.ListenUDP("udp", addr)
		ce(err, "listen")
		localConns = append(localConns, conn)
		go func() {
			buffer := make([]byte, MTU+128)
			for {
				n, remoteAddr, err := conn.ReadFromUDP(buffer)
				ce(err, "read udp")
				remoteAddrsLock.Lock()
				if len(remoteAddrs) < PORTS { // allow remote
					remoteAddrs = append(remoteAddrs, remoteAddr)
				}
				remoteAddrsLock.Unlock()
				for i, b := range buffer[:n] { // simple obfuscation
					buffer[i] = b ^ 0xDE
				}
				file.Write(buffer[:n])
			}
		}()
	}

	// read from tun
	buffer := make([]byte, MTU+128)
	for {
		n, err := file.Read(buffer)
		ce(err, "read from tun")
		for i, b := range buffer[:n] {
			buffer[i] = b ^ 0xDE
		}
		remoteAddrsLock.Lock()
		localConns[rand.Intn(len(localConns))].WriteToUDP(buffer[:n],
			remoteAddrs[rand.Intn(len(remoteAddrs))])
		remoteAddrsLock.Unlock()
	}

}

// run command
func run(cmd string, args ...string) {
	_, err := exec.Command(cmd, args...).Output()
	ce(err, "run command")
}

// info
func info(format string, args ...interface{}) {
	now := time.Now()
	fmt.Printf("%02d:%02d:%02d %s\n", now.Hour(), now.Minute(), now.Second(),
		fmt.Sprintf(format, args...))
}

func startSocksServer(addr string) {
	// start socks server
	socksServer, err := socks.New(addr)
	ce(err, "new socks5 server")
	info("Socks5 server %s started.", addr)
	inBytes := 0
	outBytes := 0
	go func() {
		for _ = range time.NewTicker(time.Second * 8).C {
			info("socks in %s out %s", formatBytes(inBytes), formatBytes(outBytes))
		}
	}()

	// handle socks client
	socksServer.OnSignal("client", func(args ...interface{}) {
		go func() {
			socksClientConn := args[0].(net.Conn)
			defer socksClientConn.Close()
			hostPort := args[1].(string)
			// dial to target addr
			conn, err := net.DialTimeout("tcp", hostPort, time.Second*32)
			if err != nil {
				info("dial error %v", err)
				return
			}
			defer conn.Close()
			// read from target
			go func() {
				buf := make([]byte, 2048)
				var err error
				var n int
				for {
					n, err = conn.Read(buf)
					inBytes += n
					socksClientConn.Write(buf[:n])
					if err != nil {
						return
					}
				}
			}()
			// read from socks client
			buf := make([]byte, 2048)
			var n int
			for {
				n, err = socksClientConn.Read(buf)
				outBytes += n
				conn.Write(buf[:n])
				if err != nil {
					return
				}
			}
		}()
	})
}

func formatBytes(n int) string {
	units := "bKMGTP"
	i := 0
	result := ""
	for n > 0 {
		res := n % 1024
		if res > 0 {
			result = fmt.Sprintf("%d%c", res, units[i]) + result
		}
		n /= 1024
		i++
	}
	if result == "" {
		return "0"
	}
	return result
}
