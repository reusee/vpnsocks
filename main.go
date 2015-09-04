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
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	socks "github.com/reusee/socks5-server"
)

const (
	MTU = 1500
)

func init() {
	var seed int64
	binary.Read(crand.Reader, binary.LittleEndian, &seed)
	rand.Seed(seed)
}

func main() {
	// parse arguments
	var isLocal bool
	var remoteAddrStr string
	listenPort := ":35555"
	for _, arg := range os.Args[1:] {
		if regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+(:[0-9]+)?`).MatchString(arg) {
			remoteAddrStr = arg
			if !strings.Contains(remoteAddrStr, ":") {
				remoteAddrStr += ":35555"
			}
			isLocal = true
		} else if regexp.MustCompile(`:[0-9]+`).MatchString(arg) {
			listenPort = arg
		} else {
			log.Fatalf("unknown argument %s", arg)
		}
	}

	// allocate tun
	var fd C.int
	var cName *C.char
	C.new_tun(&fd, &cName)
	if fd == 0 {
		log.Fatal("new_tun")
	}
	name := C.GoString(cName)
	info("fd %d dev %s", fd, name)

	// set ip
	run("ip", "link", "set", name, "up")
	ip := "192.168.168.1"
	if isLocal {
		ip = "192.168.168.2"
	}
	run("ip", "addr", "add", ip+"/24", "dev", name)
	run("ip", "link", "set", "dev", name, "mtu", strconv.Itoa(MTU))
	run("ip", "link", "set", "dev", name, "qlen", "1000")

	// socks server
	if !isLocal {
		go startSocksServer(ip)
	}

	// vars
	file := os.NewFile(uintptr(fd), name)
	remotes := make(map[string]*net.UDPAddr)

	// read from udp
	addr, err := net.ResolveUDPAddr("udp", listenPort)
	if err != nil {
		log.Fatal(err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		buffer := make([]byte, MTU+128)
		var count int
		var err error
		var remoteAddr *net.UDPAddr
		info("listening %v", addr)
		for {
			count, remoteAddr, err = conn.ReadFromUDP(buffer)
			if err != nil {
				break
			}
			if remotes[remoteAddr.String()] == nil {
				remotes[remoteAddr.String()] = remoteAddr
			}
			for i, b := range buffer[:count] {
				buffer[i] = b ^ 0xDE
			}
			file.Write(buffer[:count])
		}
	}()

	// dial to remote
	if isLocal {
		remoteAddr, err := net.ResolveUDPAddr("udp", remoteAddrStr)
		if err != nil {
			log.Fatal(err)
		}
		remotes[remoteAddr.String()] = remoteAddr
	}

	// limit
	quota := 50 * 1024
	quotaInterval := time.Millisecond * 50
	quotaLock := new(sync.Mutex)
	go func() {
		for {
			time.Sleep(quotaInterval)
			quotaLock.Lock()
			quota = 50 * 1024
			quotaLock.Unlock()
		}
	}()

	// read from tun
	buffer := make([]byte, MTU+128)
	var count int
	for {
		count, err = file.Read(buffer)
		if err != nil {
			break
		}
		for i, b := range buffer[:count] {
			buffer[i] = b ^ 0xDE
		}
		for _, remoteAddr := range remotes {
			quotaLock.Lock()
			quota -= count
			if quota < 0 {
				quotaLock.Unlock()
				time.Sleep(quotaInterval)
			} else {
				quotaLock.Unlock()
			}
			// write to udp
			conn.WriteToUDP(buffer[:count], remoteAddr)
			conn.WriteToUDP(buffer[:count], remoteAddr)
		}
	}

}

// run command
func run(cmd string, args ...string) {
	out, err := exec.Command(cmd, args...).Output()
	if err != nil {
		log.Fatalf("error on running command %s %s\n>> %s <<",
			cmd,
			strings.Join(args, " "),
			out)
	}
}

// info
func info(format string, args ...interface{}) {
	now := time.Now()
	fmt.Printf("%02d:%02d:%02d %s\n", now.Hour(), now.Minute(), now.Second(),
		fmt.Sprintf(format, args...))
}

func startSocksServer(ip string) {
	// start socks server
	addr := ip + ":1080"
	socksServer, err := socks.New(addr)
	if err != nil {
		log.Fatalf("socks5.NewServer %v", err)
	}
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
