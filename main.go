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
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var p = fmt.Printf

const (
	MTU = 1024
)

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
	p("fd %d dev %s\n", fd, name)

	// set ip
	run("ip", "link", "set", name, "up")
	ip := "10.10.10.1"
	if isLocal {
		ip = "10.10.10.2"
	}
	run("ip", "addr", "add", ip+"/24", "dev", name)
	run("ip", "link", "set", "dev", name, "mtu", strconv.Itoa(MTU))

	// vars
	file := os.NewFile(uintptr(fd), name)
	remotes := make(map[string]*net.UDPAddr)
	start := time.Now()

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
		fmt.Printf("listening %v\n", addr)
		for {
			count, remoteAddr, err = conn.ReadFromUDP(buffer)
			fmt.Printf("%v read from udp %v %v\n",
				time.Now().Sub(start),
				remoteAddr,
				count)
			if err != nil {
				break
			}
			if remotes[remoteAddr.String()] == nil {
				remotes[remoteAddr.String()] = remoteAddr
			}
			fmt.Printf("write to tun %d\n", count)
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

	// read from tun
	buffer := make([]byte, MTU+128)
	var count int
	for {
		count, err = file.Read(buffer)
		fmt.Printf("%v read from tun %v\n",
			time.Now().Sub(start),
			count)
		if err != nil {
			break
		}
		for _, remoteAddr := range remotes {
			fmt.Printf("write to %v\n", remoteAddr)
			conn.WriteToUDP(buffer[:count], remoteAddr)
		}
	}

}

func run(cmd string, args ...string) {
	out, err := exec.Command(cmd, args...).Output()
	if err != nil {
		log.Fatalf("error on running command %s %s\n>> %s <<",
			cmd,
			strings.Join(args, " "),
			out)
	}
}
