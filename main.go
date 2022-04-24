package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

var isconnect int
var group string
var id string

var work int
var rec []connect

type connect struct {
	id    string
	group string
	ip    net.IP
	port  int
	rep   bool
	isde  bool
	wip   net.IP
	wport int
}

func server(addr string) {
	work = 0
	//port := 9526

	fmt.Printf("[Server]listen on %v", addr)
	command := ""
	//addr := "127.0.0.1:" + fmt.Sprintf("%d", port)
	//addr := "0.0.0.0:" + fmt.Sprintf("%d", port)
	go udp_rec(addr)
	go heartcheck()
	//listener, err := net.Listen("tcp", addr)
	//if err != nil {
	//	log.Fatal(err)
	//}
	//defer listener.Close()

	for {
		//conn, err := listener.Accept()
		//if err != nil {
		//	log.Fatal(err)
		//}
		//go Handle_conn(conn)
		fmt.Scanf("%s\n", &command)
		if command == "stop" {
			break
		}
		if command == "help" || command == "?" || command == "" {
			fmt.Printf("Command:\nstop| stop server\n")
		}

	}

}

func heartcheck() {
	for {
		time.Sleep(5 * time.Second)
		for {
			if work == 0 {
				work = 1
				break
			}
		}
		n := len(rec)
		i := 0
		for {
			if i >= n {
				break
			}
			if !rec[i].rep {
				rec[i].isde = true
				fmt.Printf("disconnect addr=%v:%v", rec[i].ip, rec[i].port)
			} else {
				//fmt.Printf("Rest addr=%v:%v", rec[i].ip, rec[i].port)
				rec[i].rep = false
			}
			i++
		}
		i = 0
		for {
			if i >= n {
				break
			}
			if rec[i].isde {
				n--
				rec = append(rec[:i], rec[i+1:]...)
				continue
			}
			i++
		}
		work = 0
	}
}
func udp_rec(addr1 string) { //Server Handle
	udpAddr, _ := net.ResolveUDPAddr("udp", addr1)
	//UDPlistener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(0, 0, 0, 0), Port: port})
	//UDPlistener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	UDPlistener, err := net.ListenUDP("udp", udpAddr)
	if checkError(err) {
		return
	}
	fmt.Println("Using Local UDP port:", UDPlistener.LocalAddr().(*net.UDPAddr).Port)
	for {
		data := make([]byte, 1024)
		//读取
		n, addr, err := UDPlistener.ReadFromUDP(data)
		fmt.Println(addr, data[:n])
		if checkError(err) {
			continue
		}

		if data[0] == 1 { //Register

			var rr connect
			rr.group = string(data[1 : data[1]+2])
			data2 := data[data[1]+2:]
			rr.id = string(data2[1 : data2[0]+1])
			//rr.id = string(data2[data2[1]+3 : data2[1]+data2[data2[1]+2]+3])
			rr.ip = addr.IP
			rr.port = addr.Port

			fmt.Println(n, addr)
			fmt.Printf("group=%v,id=%v,ip=%v,port=%v,connection !", rr.group, rr.id, rr.ip, rr.port)
			rec = append(rec, rr)

		}
		if data[0] == 2 { //Heartbeat when recv
			a12 := 0
			var rr connect
			rr.group = string(data[1 : data[1]+2])
			datap := data[data[1]+2:]
			rr.id = string(datap[1 : datap[0]+1])

			rr.ip = addr.IP
			rr.port = addr.Port
			n := len(rec)
			i := 0
			for {
				if i >= n {
					break
				}

				if rec[i].group == rr.group && rec[i].id == rr.id {
					fmt.Printf("HeatBit rec")
					rec[i].rep = true
					UDPlistener.WriteToUDP([]byte{1}, addr)
					a12 = 1
					break
				}
				i++
			}
			if a12 == 0 {
				UDPlistener.WriteToUDP([]byte{0}, addr)
			}
			//UDPlistener.WriteToUDP([]byte{0}, addr)
			//rec = append(rec, rr)

		}
		if data[0] == 3 { //list command
			var rr connect
			rr.group = string(data[1 : data[1]+2])
			data2 := data[data[1]+2:]
			rr.id = string(data2[1 : data2[0]+1])
			rr.ip = addr.IP
			rr.port = addr.Port
			n := len(rec)
			i := 0
			var tt = []byte{0}
			var sed = []byte{2, 0}
			for {
				if i >= n {
					break
				}

				if rec[i].group == rr.group {
					fmt.Printf("List Command")
					//rec[i].rep = true
					//tt[0] = uint8(len(rr.id))
					tt[0] = uint8(len(rec[i].id))

					sed = BytesCombine(sed, tt, []byte(rec[i].id))
					sed[1] = sed[1] + 1
					UDPlistener.WriteToUDP([]byte{1}, addr)

				}
				i++
			}

			UDPlistener.WriteToUDP(sed, addr)

			//UDPlistener.WriteToUDP([]byte{0}, addr)
			//fmt.Println(n, addr)
			//fmt.Printf("group=%v,id=%v,ip=%v,port=%v,connection !", rr.group, rr.id, rr.ip, rr.port)
			//rec = append(rec, rr)

		}
		if data[0] == 4 { //Request Connect
			//fmt.Println(data2[0], "?", data2[:n])
			var rr connect
			rr.group = string(data[1 : data[1]+2])
			data2 := data[data[1]+2:]
			rr.id = string(data2[1 : data2[0]+1])
			data2 = data2[data2[0]+1:]
			//fmt.Println(data2)
			test := string(data2[1 : data2[0]+1])
			data2 = data2[data2[0]+1:]

			port_connect := int(data2[0])*256 + int(data2[1])

			if test == rr.id {
				UDPlistener.WriteToUDP([]byte{0}, addr)
			}
			fmt.Println(test, rr.id, port_connect)
			for {
				if work == 0 {
					work = 1
					break
				}
				time.Sleep(time.Second)
			}
			rr.ip = addr.IP
			rr.port = addr.Port
			n := len(rec)
			i := 0
			found := -1
			var tt = []byte{0}
			var sed = []byte{3} //Establish Sign
			for {
				if i >= n {
					break
				}

				if rec[i].group == rr.group && rec[i].id == rr.id {
					rec[i].wip = addr.IP
					rec[i].wport = addr.Port
					found = i
					//i = 0
					fmt.Printf("Connection Establish")
					fmt.Printf("Request IP and Port:%s,%d\n", rec[i].wip, rec[i].wport)
					fmt.Printf("TestStroe found=%d,i=%d\n", found, i)
					//rec[i].rep = true
					//fmt.Println(addr)
					//tt[0] = uint8(len(rr.id))
					//sed = BytesCombine(sed, tt, []byte(rr.id))
					//sed[1] = sed[1] + 1
					//UDPlistener.WriteToUDP([]byte{1}, addr)

				}
				i++
			}
			i = 0
			for {
				if i >= n {
					break
				}
				if rec[i].group == rr.group && rec[i].id == test {
					//rec[i].wip = addr.IP
					//rec[i].wport = addr.Port
					//found = 1
					//i = -1
					fmt.Printf("To Do")
					//rec[i].rep = true
					//fmt.Println(addr)
					//tt[0] = uint8(len(rr.id))
					fmt.Printf("Testinfo found=%d\n", found)
					info := rec[found].wip.String() + ":" + fmt.Sprintf("%d", rec[found].wport)
					fmt.Printf("info:%s\n", info)
					tt[0] = uint8(len(info))
					sed = BytesCombine(sed, tt, []byte(info))
					//sed[1] = sed[1] + 1

					//sendto := string(rec[i].ip) + ":" + string(rec[i].port)
					sendto := rec[i].ip.String() + ":" + fmt.Sprintf("%d", rec[i].port)

					tt[0] = uint8(found) //connect端
					sed = BytesCombine(sed, tt)

					tt[0] = uint8(i) //接受connect请求
					sed = BytesCombine(sed, tt)

					fmt.Println(info, sendto)
					udpAddr1, _ := net.ResolveUDPAddr("udp", sendto)
					fmt.Println(sed)
					UDPlistener.WriteToUDP(sed, udpAddr1)

				}

				i++

			}

			//UDPlistener.WriteToUDP(sed, addr)

			work = 0
			//UDPlistener.WriteToUDP([]byte{0}, addr)
			//fmt.Println(n, addr)
			//rec = append(rec, rr)

		}
		if data[0] == 5 { //Transfer function
			//var rr connect
			fmt.Println(data[:n])
			fist := data[1]
			second := data[2] //from this
			rec[second].wip = addr.IP
			rec[second].port = addr.Port

			udpAddr, _ := net.ResolveUDPAddr("udp", rec[fist].wip.String()+":"+fmt.Sprint(rec[fist].wport))
			var tt = []byte{0}

			info := rec[second].wip.String() + ":" + fmt.Sprintf("%d", rec[second].wport)
			tt[0] = uint8(len(info))

			UDPlistener.WriteTo(BytesCombine([]byte{5}, tt, []byte(info)), udpAddr)
			//UDPlistener.WriteToUDP(sed, addr)

			work = 0
		}

	}
}

func Handle_conn(conn net.Conn) {
	headerBuf := make([]byte, 128)
	_, err := conn.Read(headerBuf)
	if checkError(err) {
		return
	}
	if headerBuf[0] == 71 {
		var rr connect
		rr.group = string(headerBuf[0:headerBuf[1]])
		rec = append(rec, rr)

	}

}

func BytesCombine(pBytes ...[]byte) []byte {
	len := len(pBytes)
	s := make([][]byte, len)
	for index := 0; index < len; index++ {
		s[index] = pBytes[index]
	}
	sep := []byte("")
	return bytes.Join(s, sep)
}

func checkError(err error) bool {
	if err != nil {
		fmt.Println("Error:", err.Error())
		return true
	}
	return false
}

func client(addr1 string) {
	//addr := "127.0.0.1:6064"
	//flag.StringVar(&addr, "l", "127.0.0.1:6064", "Local Address 127.0.0.1:port")
	//serverdd=a
	//id=b
	//group=c
	fmt.Printf("[Client]Runing connecting to %v", addr1)
	UDPlistener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(0, 0, 0, 0), Port: 0})

	if checkError(err) {
		return
	}
	go run(UDPlistener, addr1)
	go rec1(UDPlistener, addr1)
	//command := ""
	for {

		reader := bufio.NewReader(os.Stdin)

		command1, _, err := reader.ReadLine()
		command := string(command1)
		if checkError(err) {
			continue
		}
		//fmt.Scanf("%s\n", &command)

		if command == "stop" {
			break
		}
		if command == "help" || command == "?" || command == "" {
			fmt.Printf("Command:\nstop| stop client\nlist |list of user\n")
			continue
		}

		if command == "list" {
			var gro []byte = []byte(group)
			var tt = []byte{0}
			var iid []byte = []byte(id)
			tt[0] = uint8(len(group))
			gro = BytesCombine([]byte{3}, tt, gro)
			tt[0] = uint8(len(id))
			gro = BytesCombine(gro, tt, iid)

			addr, err := net.ResolveUDPAddr("udp", addr1)
			if checkError(err) {
				continue
			}
			UDPlistener.WriteTo(gro, addr)
			continue
		}
		arr := strings.Fields(command)
		//fmt.Println(arr)
		if len(arr) > 2 {
			if arr[0] == "connect2" {
				var gro []byte = []byte(group)
				var tt = []byte{0}
				var iid []byte = []byte(id)
				tt[0] = uint8(len(group))
				gro = BytesCombine([]byte{4}, tt, gro)
				tt[0] = uint8(len(id))
				gro = BytesCombine(gro, tt, iid)

				iid = []byte(arr[1])
				tt[0] = uint8(len(arr[1]))

				gro = BytesCombine(gro, tt, iid)
				tt = []byte{0, 0}
				var ii, err = strconv.Atoi(arr[2])
				tt[0] = uint8(ii >> 8)
				tt[1] = uint8(ii)
				gro = BytesCombine(gro, tt)
				//fmt.Println(gro)
				//addr, err := net.ResolveUDPAddr("udp", addr1)
				if checkError(err) {
					continue
				}
				go Establish_UDP_tunl(gro, addr1, ii)
				//UDPlistener.WriteTo(gro, addr)
				continue

			}
		} else {
			fmt.Printf("Wrong Command")
		}
	}

}

func Establish_UDP_tunl(gro []byte, addr1 string, port int) {
	//data := make([]byte, 1024)
	//UDPlistener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	UDPlistener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(0, 0, 0, 0), Port: 0})
	fmt.Printf("Tunl Run on Port:%s,Plase connect with this port\n", UDPlistener.LocalAddr().String())
	if checkError(err) {
		return
	}
	addr, err := net.ResolveUDPAddr("udp", addr1)
	if checkError(err) {
		return
	}
	UDPlistener.WriteTo(gro, addr)
	var localaddr net.Addr
	var remoteaddr net.Addr
	for {

		data2 := make([]byte, 1024)
		//读取
		n, addr, err := UDPlistener.ReadFrom(data2)

		//fmt.Println(addr, data2[:n])
		fmt.Printf("Testing Address in %s\n", addr.String())
		if strings.Split(addr.String(), ":")[0] != "127.0.0.1" {
			if addr == nil {
				break
			}
			if checkError(err) {
				break
			}
			data := data2[:n]
			remoteaddr = addr
			if localaddr != nil {
				UDPlistener.WriteTo(data[:n], localaddr)
			} else {
				if data[0] == 4 {
					tt := []byte{0, 0}

					tt[0] = uint8(port >> 8)
					tt[1] = uint8(port)
					UDPlistener.WriteTo(tt, addr)
				}
				if data[0] == 5 {
					tt := []byte{0, 0}

					tt[0] = uint8(port >> 8)
					tt[1] = uint8(port)

					data = data[1:]

					len := data[0]
					addrP := string(data[1:len])
					addrPP, err := net.ResolveUDPAddr("udp", addrP)
					if checkError(err) {
						return
					}
					UDPlistener.WriteTo(tt, addrPP)
				}
			}

			fmt.Printf("\nn=%d\n", n)
			fmt.Println(data)
		} else {
			localaddr = addr
			//fmt.Println("Ready to Send to Remote:\n")
			fmt.Println(data2)

			//gro = BytesCombine(tt, data2[:n])
			UDPlistener.WriteTo(data2[:n], remoteaddr)
		}
	}
	/**
		n, client, err := UDPlistener.ReadFromUDP(data)
		fmt.Println(n, client, err)
		if checkError(err) {
			fmt.Println("Wrong Connecting Lenth:", n, client)
			return
		}
		if n < 2 {
			fmt.Printf("connect failed\ncan not connect self\n")
			return
		}
	**/
}

func run(UDPlistener *net.UDPConn, addr1 string) {

	fmt.Println("Using Local UDP port:", UDPlistener.LocalAddr().(*net.UDPAddr).IP, UDPlistener.LocalAddr().(*net.UDPAddr).Port)
	for {
		//var addr net.UDPAddr
		//addr.IP = net.IP(strings.Split(serverdd, ":")[0])
		//addr.Port, err = strconv.Atoi(strings.Split(serverdd, ":")[1])
		addr, err := net.ResolveUDPAddr("udp", addr1)
		//fmt.Println(addr.String())
		if checkError(err) {
			time.Sleep(4 * time.Second)
			continue
		}
		//fmt.Printf("%v:%v", &addr.IP, addr.Port)
		var gro []byte = []byte(group)
		var tt = []byte{0}
		var iid []byte = []byte(id)
		tt[0] = uint8(len(group))
		gro = BytesCombine([]byte{1}, tt, gro)
		tt[0] = uint8(len(id))
		gro = BytesCombine(gro, tt, iid)
		//fmt.Println(gro)
		UDPlistener.WriteTo(gro, addr)
		gro[0] = uint8(2)
		isconnect = 1
		for {
			UDPlistener.WriteToUDP(gro, addr)
			time.Sleep(4 * time.Second)
			//fmt.Println(addr, gro)
			if isconnect == 0 {
				break
			}
		}

	}

}
func rec1(UDPlistener *net.UDPConn, addrc string) {
	for {
		data2 := make([]byte, 1024)
		//读取
		n, addr, err := UDPlistener.ReadFrom(data2)
		//fmt.Println(addr, data2[:n])
		if addr == nil {
			break
		}
		if checkError(err) {
			continue
		}
		if data2[0] == 0 { //失败
			isconnect = 0

		}
		if data2[0] == 1 { //成功
			isconnect = 1
			//fmt.Println("Heart Bit")
		}
		if data2[0] == 2 { //list返回
			fmt.Println(data2[:n])
			data := data2[2:]
			for pp := 0; uint8(pp) < data2[1]; pp++ {
				fmt.Printf("%d.%s\n", pp, string(data[1:data[0]+1]))
				data = data[data[0]+1:]
				//fmt.Println(data)

			}
			//isconnect = 1

		}
		if data2[0] == 3 { //Request Connect
			fmt.Println(data2[:n])
			data := data2[1:n]
			test := data[1 : data[0]+1]
			fmt.Println(test, data[data[0]+1], data[data[0]+2], data[len(data)-2:])
			//fmt.Println(string(test))
			go Establish_UDP_tunl2(string(test), data[len(data)-2:], addrc)

			//addr, _ := net.ResolveUDPAddr("udp", string(test))
			//UDPlistener.WriteToUDP([]byte{4, 0}, addr)
			//fmt.Printf(addr.String())

			//for pp := 0; uint8(pp) < data2[1]; pp++ {
			//	fmt.Printf("%d.%s\n", pp, string(data[1:data[0]+1]))
			//	data = data[data[0]+1:]
			//fmt.Println(data)

			//}

			//isconnect = 1

		}
		//if data2[0] == 4 { //Data From Remote
		//	data := data2[1:n]
		//	fmt.Println(data)
		//}
		//time.Sleep(10)
	}
}

func Establish_UDP_tunl2(addr1 string, trans []byte, addrc string) {
	//data := make([]byte, 1024)
	//UDPlistener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	UDPlistener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(0, 0, 0, 0), Port: 0})
	fmt.Printf("Waitting for data from:%s,Ready to connectt\n", UDPlistener.LocalAddr().String())
	if checkError(err) {
		return
	}
	addr, err := net.ResolveUDPAddr("udp", addr1)
	if checkError(err) {
		return
	}
	UDPlistener.WriteTo([]byte{4, 0}, addr)
	udpAddr, _ := net.ResolveUDPAddr("udp", addrc)
	UDPlistener.WriteTo(BytesCombine([]byte{5}, trans), udpAddr)

	var port int
	portinfo := make([]byte, 1024)
	nn, c, errc := UDPlistener.ReadFrom(portinfo)
	if checkError(errc) {
		return
	}
	if nn < 1 {
		return
	}
	if c == nil {
		return
	}
	port = int(portinfo[0])*256 + int(portinfo[1])

	for {

		//data2 := make([]byte, 1024)
		data2 := make([]byte, 1024)
		//读取
		n, addr, err := UDPlistener.ReadFrom(data2)

		//fmt.Println(addr, data2[:n])
		fmt.Printf("Testing Address in %s\n", addr.String())

		if strings.Split(addr.String(), ":")[0] != "127.0.0.1" {
			if addr == nil {
				break
			}
			if checkError(err) {
				break
			}
			data := data2[:n]
			fmt.Printf("\nn=%d\n", n)
			fmt.Println(data)
			//sendaddr, _ := net.ResolveUDPAddr("udp", fmt.Sprintf("127.0.0.1:%d", int(data[0])*256+int(data[1])))
			sendaddr, _ := net.ResolveUDPAddr("udp", fmt.Sprintf("127.0.0.1:%d", port))
			UDPlistener.WriteTo(data[2:n], sendaddr)

		} else {
			//fmt.Println("Ready to Send to Remote:\n")
			fmt.Println(data2)
			UDPlistener.WriteTo(data2[:n], addr)

		}
	}
	/**
		n, client, err := UDPlistener.ReadFromUDP(data)
		fmt.Println(n, client, err)
		if checkError(err) {
			fmt.Println("Wrong Connecting Lenth:", n, client)
			return
		}
		if n < 2 {
			fmt.Printf("connect failed\ncan not connect self\n")
			return
		}
	**/
}

func main() {
	var init string
	var serverdd string
	//port := 9265
	flag.StringVar(&init, "mod", "server", "server or client default servermod")
	flag.StringVar(&serverdd, "c", "127.0.0.1:9526", "Clientmod:Connect to Server server:port\nServermod: Listen Port")
	flag.StringVar(&id, "u", "user0", "User")
	flag.StringVar(&group, "g", "0", "group")

	//flag.IntVar(&port, "l", 9526, "Local Address port")

	flag.Parse()

	if init != "client" {
		server(serverdd)

	} else {
		if group == "0" {
			flag.Usage()
			fmt.Printf("\ngroup can not be 0\n")
			return
		}
		client(serverdd)
	}
}
