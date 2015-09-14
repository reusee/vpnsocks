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
	"sync/atomic"
	"time"

	socks "github.com/reusee/socks5-server"
)

const (
	MTU   = 1500
	PORTS = 8
)

var (
	sp = fmt.Sprintf
	pt = fmt.Printf
)

func init() {
	var seed int64
	binary.Read(crand.Reader, binary.LittleEndian, &seed)
	rand.Seed(seed)
}

func main() {
	if len(os.Args) < 3 {
		pt("usage: %s [config file path] [key file path]\n", os.Args[0])
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
		pt("must be %d local ports\n", PORTS)
		return
	}
	if len(config.RemotePorts) != 0 && len(config.RemotePorts) != PORTS {
		pt("must be zero or %d remote ports\n", PORTS)
		return
	}
	if config.LocalAddr == "" {
		config.LocalAddr = "0.0.0.0"
	}
	if config.Socks5Port == 0 {
		config.Socks5Port = 1080
	}
	isLocal := len(config.RemotePorts) > 0
	if isLocal && config.RemoteAddr == "" {
		pt("remote addr must not be empty\n")
		return
	}

	// read key file
	key, err := ioutil.ReadFile(os.Args[2])
	ce(err, "read key file")
	if len(key) != 65536 {
		pt("invalid key file\n")
		return
	}

	// setup tun device
	var fd C.int
	var cName *C.char
	C.new_tun(&fd, &cName)
	if fd == 0 {
		panic("allocate tun")
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
	var addrs []*net.UDPAddr
	for _, port := range config.RemotePorts {
		addr, err := net.ResolveUDPAddr("udp", sp("%s:%d", config.RemoteAddr, port))
		ce(err, "resolve addr")
		addrs = append(addrs, addr)
	}
	var remoteAddrs atomic.Value
	remoteAddrs.Store(addrs)

	// sender
	packets := make(chan []byte)
	go func() {
		ticker := time.NewTicker(time.Millisecond * 100)
		n := 128 * 1024
		for {
			select {
			case bs := <-packets:
				if n > 0 {
					_, err := file.Write(bs)
					ce(err, "write to tun")
					n -= len(bs)
				} else {
				}
			case <-ticker.C:
				n = 128 * 1024
			}
		}
	}()

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
				addrs := remoteAddrs.Load().([]*net.UDPAddr)
				if len(addrs) < PORTS { // allow remote
					newAddrs := make([]*net.UDPAddr, len(addrs))
					copy(newAddrs, addrs)
					newAddrs = append(newAddrs, remoteAddr)
					remoteAddrs.Store(newAddrs)
					info("remote %v added.", remoteAddr)
				}
				keyIndex := n
				var lastByte byte
				for i, b := range buffer[:n] { // simple obfuscation
					keyIndex += i + int(lastByte)
					keyIndex %= 65536
					b = b ^ key[keyIndex]
					lastByte = buffer[i]
					buffer[i] = b
				}
				packets <- buffer[:n]
			}
		}()
	}

	// read from tun
	buffer := make([]byte, MTU+128)
	for {
		n, err := file.Read(buffer)
		ce(err, "read from tun")
		keyIndex := n
		var lastByte byte
		for i, b := range buffer[:n] {
			keyIndex += i + int(lastByte)
			keyIndex %= 65536
			b = b ^ key[keyIndex]
			lastByte = b
			buffer[i] = b
		}
		addrs := remoteAddrs.Load().([]*net.UDPAddr)
		if len(addrs) == 0 {
			continue
		}
		localConns[rand.Intn(len(localConns))].WriteToUDP(buffer[:n],
			addrs[rand.Intn(len(addrs))])
		if rand.Intn(100) < 50 {
			localConns[rand.Intn(len(localConns))].WriteToUDP(buffer[:n],
				addrs[rand.Intn(len(addrs))])
		}
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
	pt("%02d:%02d:%02d %s\n", now.Hour(), now.Minute(), now.Second(),
		sp(format, args...))
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
			result = sp("%d%c", res, units[i]) + result
		}
		n /= 1024
		i++
	}
	if result == "" {
		return "0"
	}
	return result
}
