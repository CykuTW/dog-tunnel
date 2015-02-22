package main

import (
	"./common"
	"./pipe"
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	_ "crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"os/user"
	"path"
	//"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//var cpuprofile = flag.String("cpuprofile", "", "write cpu profile to file")

var authKey = flag.String("auth", "", "key for auth")
var pipeN = flag.Int("pipe", 1, "pipe num(todo...)")
var bTcp = flag.Bool("tcp", false, "use tcp to replace udp")
var xorData = flag.String("xor", "", "xor key,c/s must use a some key")

var serviceAddr = flag.String("service", "", "listen addr for client connect")
var localAddr = flag.String("local", "", "if local not empty, treat me as client, this is the addr for local listen, otherwise, treat as server")
var remoteAction = flag.String("action", "socks5", "for client control server, if action is socks5,remote is socks5 server, if is addr like 127.0.0.1:22, remote server is a port redirect server")
var bVerbose = flag.Bool("v", false, "verbose mode")
var bShowVersion = flag.Bool("version", false, "show version")
var bEncrypt = flag.Bool("encrypt", false, "p2p mode encrypt")
var dnsCacheNum = flag.Int("dnscache", 0, "if > 0, dns will cache xx minutes")
var timeOut = flag.Int("timeout", 100, "udp pipe set timeout(seconds)")

var bDebug = flag.Bool("debug", false, "more output log")
var bReverse = flag.Bool("r", false, "reverse mode, if true, client 's \"-local\" address will be listened on server side")

var clientType = 1

const maxPipes = 10

var clientReportSessionChan chan bool

type dnsInfo struct {
	Ip                  string
	Status              string
	Queue               []*dnsQueryReq
	overTime, cacheTime int64
}

func debug(args ...interface{}) {
	if *bDebug {
		log.Println(args...)
	}
}

func (u *dnsInfo) IsAlive() bool {
	return time.Now().Unix() < u.overTime
}

func (u *dnsInfo) SetCacheTime(t int64) {
	if t >= 0 {
		u.cacheTime = t
	} else {
		t = u.cacheTime
	}
	u.overTime = t + time.Now().Unix()
}

func (u *dnsInfo) GetCacheTime() int64 {
	return u.overTime
}

func (u *dnsInfo) DeInit() {}

var g_ClientMap map[string]*Client
var markName = ""
var bForceQuit = false

func iclock() int32 {
	return int32((time.Now().UnixNano() / 1000000) & 0xffffffff)
}

func getEncodeFunc(aesBlock cipher.Block) func([]byte) []byte {
	return func(s []byte) []byte {
		if aesBlock == nil {
			return s
		} else {
			padLen := aes.BlockSize - (len(s) % aes.BlockSize)
			for i := 0; i < padLen; i++ {
				s = append(s, byte(padLen))
			}
			srcLen := len(s)
			encryptText := make([]byte, srcLen+aes.BlockSize)
			iv := encryptText[srcLen:]
			for i := 0; i < len(iv); i++ {
				iv[i] = byte(i)
			}
			mode := cipher.NewCBCEncrypter(aesBlock, iv)
			mode.CryptBlocks(encryptText[:srcLen], s)
			return encryptText
		}
	}
}

func getDecodeFunc(aesBlock cipher.Block) func([]byte) []byte {
	return func(s []byte) []byte {
		if aesBlock == nil {
			return s
		} else {
			if len(s) < aes.BlockSize*2 || len(s)%aes.BlockSize != 0 {
				return []byte{}
			}
			srcLen := len(s) - aes.BlockSize
			decryptText := make([]byte, srcLen)
			iv := s[srcLen:]
			mode := cipher.NewCBCDecrypter(aesBlock, iv)
			mode.CryptBlocks(decryptText, s[:srcLen])
			paddingLen := int(decryptText[srcLen-1])
			if paddingLen > 16 {
				return []byte{}
			}
			return decryptText[:srcLen-paddingLen]
		}
	}
}

func CreateSessionAndLoop(bIsTcp bool, idindex int) {
	CreateSession(bIsTcp, idindex)
	time.AfterFunc(time.Second*3, func() {
		CreateSessionAndLoop(bIsTcp, idindex)
	})
	log.Println("sys will reconnect pipe", idindex, "after 3 seconds")
}

func CreateSession(bIsTcp bool, idindex int) bool {
	var s_conn net.Conn
	var err error
	if bIsTcp {
		s_conn, err = net.DialTimeout("tcp", *serviceAddr, 30*time.Second)
	} else {
		s_conn, err = pipe.DialTimeout(*serviceAddr, *timeOut)
	}
	if err != nil {
		log.Println("try dial err", err.Error())
		return false
	}
	log.Println("try dial", *serviceAddr, "ok", idindex, s_conn.LocalAddr().String())
	id := *serviceAddr
	client, bHave := g_ClientMap[id]
	if !bHave {
		client = &Client{id: id, ready: true, bUdp: !bIsTcp, sessions: make(map[string]*clientSession), pipes: make(map[int]net.Conn), quit: make(chan bool), pipesInfo: make(map[int]*pipeInfo)}
		g_ClientMap[id] = client
	}
	if idindex == 0 {
		if *authKey != "" {
			log.Println("request auth key", *authKey)
			common.Write(s_conn, "-1", "auth", common.Xor(*authKey))
		}
		client.authed = true
		if *bEncrypt {
			log.Println("request encrypt")
			encrypt_tail := string([]byte(fmt.Sprintf("%d%d", int32(time.Now().Unix()), (rand.Intn(100000) + 100)))[:12])
			aesKey := "asd4" + encrypt_tail
			log.Println("debug aeskey", encrypt_tail)
			aesBlock, _ := aes.NewCipher([]byte(aesKey))
			common.Write(s_conn, "-1", "init_enc", common.Xor(encrypt_tail))
			client.SetCrypt(getEncodeFunc(aesBlock), getDecodeFunc(aesBlock))
		}
		client.reverseAddr = *localAddr
		client.action = *remoteAction
		common.WriteCrypt(s_conn, "-1", "init_action", *remoteAction, client.encode)
	}
	client.pipes[idindex] = s_conn
	pinfo := &pipeInfo{0, 0, 0}
	client.pipesInfo[idindex] = pinfo
	clientReportSessionChan <- true
	pinfo.t = time.Now().Unix()
	callback := func(conn net.Conn, sessionId, action, content string) {
		t := time.Now().Unix()
		if t-pinfo.t < 60 {
			pinfo.bytes += len(content)
			pinfo.times = int(t - pinfo.t)
		} else {
			pinfo.t = t
			pinfo.bytes = len(content)
			pinfo.times = 1
		}
		if client.decode != nil {
			content = string(client.decode([]byte(content)))
		}
		client.OnTunnelRecv(conn, sessionId, action, content)
	}
	if bIsTcp {
		common.Read(s_conn, callback)
	} else {
		common.ReadUDP(s_conn, callback, pipe.ReadBufferSize)
	}
	log.Println("remove pipe", idindex)
	clientReportSessionChan <- false
	delete(client.pipes, idindex)
	delete(client.pipesInfo, idindex)
	if len(client.pipes) == 0 {
		client.Quit()
	}
	return true
}
func Listen(bIsTcp bool, addr string) bool {
	var err error
	if bIsTcp {
		g_LocalConn, err = net.Listen("tcp", addr)
	} else {
		g_LocalConn, err = pipe.Listen(addr)
	}
	if err != nil {
		g_LocalConn = nil
		log.Println("cannot listen addr:" + err.Error())
		return false
	}
	println("service start success,please connect", addr)
	for {
		conn, err := g_LocalConn.Accept()
		if err != nil {
			log.Println("server err:", err.Error())
			break
		}
		//log.Println("client", sc.id, "create session", sessionId)

		id := conn.RemoteAddr().String()
		if bIsTcp {
			log.Println("add tcp session", id)
		} else {
			log.Println("add udp session", id)
		}
		client, have := g_ClientMap[id]
		if !have {
			client = &Client{id: id, ready: true, bUdp: bIsTcp, sessions: make(map[string]*clientSession), pipes: make(map[int]net.Conn), quit: make(chan bool), pipesInfo: make(map[int]*pipeInfo)}
			g_ClientMap[id] = client
			if *authKey == "" {
				client.authed = true
			}
		}

		idindex := -1
		for i := 0; i < maxPipes; i++ {
			_, bHave := client.pipes[i]
			if !bHave {
				idindex = i
				client.pipes[i] = conn
				client.pipesInfo[i] = &pipeInfo{0, 0, 0}
				break
			}
		}
		if idindex == -1 {
			log.Println("cannot over max pipes", maxPipes, "for", id)
		} else {
			log.Println("add pipe", idindex, "for", id)
		}
		go client.ServerProcess(bIsTcp, idindex)
	}
	g_LocalConn = nil
	return true
}

func (client *Client) ServerProcess(bIsTcp bool, idindex int) {
	f := func() {
		if client.owner != nil {
			old := client.id
			delete(g_ClientMap, old)
			idindex = client.newindex
			client = client.owner
			log.Println(old, "pipe >>", client.id, idindex)
		}
	}
	callback := func(conn net.Conn, sessionId, action, content string) {
		f()
		if client.decode != nil {
			content = string(client.decode([]byte(content)))
		}
		client.OnTunnelRecv(conn, sessionId, action, content)
	}
	if bIsTcp {
		common.Read(client.pipes[idindex], callback)
	} else {
		common.ReadUDP(client.pipes[idindex], callback, pipe.ReadBufferSize)
	}
	f()
	delete(client.pipes, idindex)
	delete(client.pipesInfo, idindex)
	if bIsTcp {
		log.Println("remove tcp pipe", idindex, "for", client.id)
	} else {
		log.Println("remove udp pipe", idindex, "for", client.id)
	}
	if len(client.pipes) == 0 {
		client.Quit()
	}
}

type fileSetting struct {
	Key string
}

func saveSettings(info fileSetting) error {
	clientInfoStr, err := json.Marshal(info)
	if err != nil {
		return err
	}
	user, err := user.Current()
	if err != nil {
		return err
	}
	filePath := path.Join(user.HomeDir, ".dtunnel")

	return ioutil.WriteFile(filePath, clientInfoStr, 0700)
}

func loadSettings(info *fileSetting) error {
	user, err := user.Current()
	if err != nil {
		return err
	}
	filePath := path.Join(user.HomeDir, ".dtunnel")
	content, err := ioutil.ReadFile(filePath)
	if err != nil {
		return err
	}
	err = json.Unmarshal([]byte(content), info)
	if err != nil {
		return err
	}
	return nil
}

var checkDns chan *dnsQueryReq
var checkDnsRes chan *dnsQueryBack

type dnsQueryReq struct {
	c       chan *dnsQueryRes
	host    string
	port    int
	reqtype string
	url     string
}

type dnsQueryBack struct {
	host   string
	status string
	conn   net.Conn
	err    error
}

type dnsQueryRes struct {
	conn net.Conn
	err  error
	ip   string
}

func dnsLoop() {
	for {
		select {
		case info := <-checkDns:
			cache := common.GetCacheContainer("dns")
			cacheInfo := cache.GetCache(info.host)
			if cacheInfo == nil {
				cache.AddCache(info.host, &dnsInfo{Queue: []*dnsQueryReq{info}, Status: "querying"}, int64(*dnsCacheNum*60))
				go func() {
					back := &dnsQueryBack{host: info.host}
					//log.Println("try dial", info.url)
					s_conn, err := net.DialTimeout(info.reqtype, info.url, 30*time.Second)
					//log.Println("try dial", info.url, "ok")
					if err != nil {
						back.status = "queryfail"
						back.err = err
					} else {
						back.status = "queryok"
						back.conn = s_conn
					}
					checkDnsRes <- back
				}()
			} else {
				_cacheInfo := cacheInfo.(*dnsInfo)
				debug("on trigger", info.host, _cacheInfo.GetCacheTime(), len(_cacheInfo.Queue))
				switch _cacheInfo.Status {
				case "querying":
					_cacheInfo.Queue = append(_cacheInfo.Queue, info)
					//cache.UpdateCache(info.host, _cacheInfo)
					cacheInfo.SetCacheTime(-1)
				case "queryok":
					cacheInfo.SetCacheTime(-1)
					go func() {
						info.c <- &dnsQueryRes{ip: _cacheInfo.Ip}
					}()
				}
				//url = cacheInfo.(*dnsInfo).Ip + fmt.Sprintf(":%d", info.port)
			}
		case info := <-checkDnsRes:
			cache := common.GetCacheContainer("dns")
			cacheInfo := cache.GetCache(info.host)
			if cacheInfo != nil {
				_cacheInfo := cacheInfo.(*dnsInfo)
				_cacheInfo.Status = info.status
				switch info.status {
				case "queryfail":
					for _, _info := range _cacheInfo.Queue {
						c := _info.c
						go func() {
							c <- &dnsQueryRes{err: info.err}
						}()
					}
					cache.DelCache(info.host)
				case "queryok":
					log.Println("add host", info.host, "to dns cache")
					_cacheInfo.Ip = strings.Split(info.conn.RemoteAddr().String(), ":")[0]
					_cacheInfo.SetCacheTime(-1)
					debug("process the queue of host", info.host, len(_cacheInfo.Queue))
					conn := info.conn
					for _, _info := range _cacheInfo.Queue {
						c := _info.c
						go func() {
							c <- &dnsQueryRes{ip: _cacheInfo.Ip, conn: conn}
						}()
						conn = nil
					}
					_cacheInfo.Queue = []*dnsQueryReq{}
				}
				//cache.UpdateCache(info.host, _cacheInfo)
			}
		}
	}
}

func main() {
	flag.Parse()
	/*if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}*/
	rand.Seed(time.Now().Unix())
	checkDns = make(chan *dnsQueryReq)
	checkDnsRes = make(chan *dnsQueryBack)
	go dnsLoop()
	if *bShowVersion {
		fmt.Printf("%.2f\n", common.Version)
		return
	}
	if !*bVerbose {
		log.SetOutput(ioutil.Discard)
	}
	if *serviceAddr == "" {
		println("you must assign service arg")
		return
	}
	if *localAddr == "" {
		clientType = 0
	}
	if clientType == 1 && *pipeN > maxPipes {
		println("pipe need <=", maxPipes)
		return
	}
	if *bEncrypt {
		if clientType != 1 {
			println("only client side need encrypt")
			return
		}
	}
	if *remoteAction == "" && clientType == 1 {
		println("must have action")
		return
	}
	if *xorData != "" {
		common.XorSetKey(*xorData)
	}
	g_ClientMap = make(map[string]*Client)
	var w sync.WaitGroup
	w.Add(2)
	pipen := 0
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		n := 0
		f := func() {
			<-c
			n++
			if n > 2 {
				log.Println("force shutdown")
				os.Exit(-1)
			}
			log.Println("received signal,shutdown")
			bForceQuit = true
			for _, client := range g_ClientMap {
				client.Quit()
				pipen = 0
			}
			if g_LocalConn != nil {
				g_LocalConn.Close()
			}
		}
		f()
		go func() {
			for {
				f()
			}
		}()
		w.Done()
	}()

	loop := func() {
		if clientType == 0 {
			Listen(*bTcp, *serviceAddr)
			if !bForceQuit {
				w.Done()
			}
			w.Done()
		} else {
			clientReportSessionChan = make(chan bool)
			bDropFromZero := true
			go func() {
				for {
					select {
					case r := <-clientReportSessionChan:
						if r {
							pipen++
							if pipen == *pipeN && bDropFromZero {
								client, bHave := g_ClientMap[*serviceAddr]
								if bHave {
									pipe, bH := client.pipes[0]
									if !bH {
										log.Println("error!,no pipe 0")
										client.Quit()
									} else {
										common.WriteCrypt(pipe, "-1", "ready", "", client.encode)
									}
								}
							}
						} else {
							pipen--
							if pipen <= 0 {
								bDropFromZero = true
								pipen = 0
								client, bHave := g_ClientMap[*serviceAddr]
								if bHave {
									client.Quit()
								}
							} else {
								bDropFromZero = false
							}
						}
					}
				}
			}()
			for i := 0; i < *pipeN; i++ {
				go CreateSessionAndLoop(*bTcp, i)
			}
			w.Done()
		}
	}
	loop()
	w.Wait()
	log.Println("service shutdown")
}

type clientSession struct {
	pipe      net.Conn
	localConn net.Conn
	status    string
	recvMsg   string
	extra     uint8
	quit      chan bool
}

func (session *clientSession) processSockProxy(sessionId, content string, callback func([]byte)) {
	session.recvMsg += content
	bytes := []byte(session.recvMsg)
	size := len(bytes)
	//log.Println("recv msg-====", len(session.recvMsg),  session.status, sessionId)
	switch session.status {
	case "init":
		if size < 2 {
			//println("wait init")
			return
		}
		var _, nmethod uint8 = bytes[0], bytes[1]
		session.status = "version"
		session.recvMsg = string(bytes[2:])
		session.extra = nmethod
	case "version":
		if uint8(size) < session.extra {
			//println("wait version")
			return
		}
		var send = []uint8{5, 0}
		go session.localConn.Write(send)
		session.status = "hello"
		session.recvMsg = string(bytes[session.extra:])
		session.extra = 0
	case "hello":
		var hello reqMsg
		bOk, tail := hello.read(bytes)
		if bOk {
			session.status = "ok"
			session.recvMsg = string(tail)
			callback(bytes)
		}
		return
	case "ok":
		return
	}
	session.processSockProxy(sessionId, "", callback)
}

type ansMsg struct {
	ver  uint8
	rep  uint8
	rsv  uint8
	atyp uint8
	buf  [300]uint8
	mlen uint16
}

func (msg *ansMsg) gen(req *reqMsg, rep uint8) {
	msg.ver = 5
	msg.rep = rep //rfc1928
	msg.rsv = 0
	msg.atyp = 1 //req.atyp

	msg.buf[0], msg.buf[1], msg.buf[2], msg.buf[3] = msg.ver, msg.rep, msg.rsv, msg.atyp
	for i := 5; i < 11; i++ {
		msg.buf[i] = 0
	}
	msg.mlen = 10
}

type reqMsg struct {
	ver       uint8     // socks v5: 0x05
	cmd       uint8     // CONNECT: 0x01, BIND:0x02, UDP ASSOCIATE: 0x03
	rsv       uint8     //RESERVED
	atyp      uint8     //IP V4 addr: 0x01, DOMANNAME: 0x03, IP V6 addr: 0x04
	dst_addr  [255]byte //
	dst_port  [2]uint8  //
	dst_port2 uint16    //

	reqtype string
	url     string
}

func (msg *reqMsg) read(bytes []byte) (bool, []byte) {
	size := len(bytes)
	if size < 4 {
		return false, bytes
	}
	buf := bytes[0:4]

	msg.ver, msg.cmd, msg.rsv, msg.atyp = buf[0], buf[1], buf[2], buf[3]
	//println("test", msg.ver, msg.cmd, msg.rsv, msg.atyp)

	if 5 != msg.ver || 0 != msg.rsv {
		log.Println("Request Message VER or RSV error!")
		return false, bytes[4:]
	}
	buf = bytes[4:]
	size = len(buf)
	switch msg.atyp {
	case 1: //ip v4
		if size < 4 {
			return false, buf
		}
		copy(msg.dst_addr[:4], buf[:4])
		buf = buf[4:]
		size = len(buf)
	case 4:
		if size < 16 {
			return false, buf
		}
		copy(msg.dst_addr[:16], buf[:16])
		buf = buf[16:]
		size = len(buf)
	case 3:
		if size < 1 {
			return false, buf
		}
		copy(msg.dst_addr[:1], buf[:1])
		buf = buf[1:]
		size = len(buf)
		if size < int(msg.dst_addr[0]) {
			return false, buf
		}
		copy(msg.dst_addr[1:], buf[:int(msg.dst_addr[0])])
		buf = buf[int(msg.dst_addr[0]):]
		size = len(buf)
	}
	if size < 2 {
		return false, buf
	}
	copy(msg.dst_port[:], buf[:2])
	msg.dst_port2 = (uint16(msg.dst_port[0]) << 8) + uint16(msg.dst_port[1])

	switch msg.cmd {
	case 1:
		msg.reqtype = "tcp"
	case 2:
		log.Println("BIND")
	case 3:
		msg.reqtype = "udp"
	}
	switch msg.atyp {
	case 1: // ipv4
		msg.url = fmt.Sprintf("%d.%d.%d.%d:%d", msg.dst_addr[0], msg.dst_addr[1], msg.dst_addr[2], msg.dst_addr[3], msg.dst_port2)
	case 3: //DOMANNAME
		msg.url = string(msg.dst_addr[1 : 1+msg.dst_addr[0]])
		msg.url += fmt.Sprintf(":%d", msg.dst_port2)
	case 4: //ipv6
		msg.url = fmt.Sprintf("%x%x:%x%x:%x%x:%x%x:%x%x:%x%x:%x%x:%x%x:%d", msg.dst_addr[0], msg.dst_addr[1], msg.dst_addr[2], msg.dst_addr[3],
			msg.dst_addr[4], msg.dst_addr[5], msg.dst_addr[6], msg.dst_addr[7],
			msg.dst_addr[8], msg.dst_addr[9], msg.dst_addr[10], msg.dst_addr[11],
			msg.dst_addr[12], msg.dst_addr[13], msg.dst_addr[14], msg.dst_addr[15],
			msg.dst_port2)
	}
	log.Println(msg.reqtype, msg.url, msg.atyp, msg.dst_port2)
	return true, buf[2:]
}

type pipeInfo struct {
	bytes int
	times int
	t     int64
}

type Client struct {
	id             string
	buster         bool
	pipes          map[int]net.Conn          // client for pipes
	pipesInfo      map[int]*pipeInfo         // client for pipes
	sessions       map[string]*clientSession // session to pipeid
	ready          bool
	bUdp           bool
	action         string
	quit           chan bool
	encode, decode func([]byte) []byte
	authed         bool
	localconn      net.Conn
	listener       net.Listener
	reverseAddr    string
	owner          *Client
	readyId        string
	newindex       int
}

// pipe : client to client
// local : client to local apps
func (sc *Client) getSession(sessionId string) *clientSession {
	session, _ := sc.sessions[sessionId]
	return session
}

func (sc *Client) removeSession(sessionId string) bool {
	common.RmId("udp", sessionId)
	session, bHave := sc.sessions[sessionId]
	if bHave {
		if session.quit != nil {
			close(session.quit)
			session.quit = nil
		}
		if session.localConn != nil {
			session.localConn.Close()
		}
		delete(sc.sessions, sessionId)
		//log.Println("client", sc.id, "remove session", sessionId)
		return true
	}
	return false
}

func (sc *Client) OnTunnelRecv(pipe net.Conn, sessionId string, action string, content string) {
	debug("recv p2p tunnel", sessionId, action, len(content))
	session := sc.getSession(sessionId)
	var conn net.Conn
	if session != nil {
		conn = session.localConn
	}
	if clientType == 0 && !sc.authed && action != "collect" {
		if action != "auth" || common.Xor(content) != *authKey {
			log.Println("auth fail", action, common.Xor(content), *authKey, pipe.RemoteAddr().String())
			go common.Write(pipe, sessionId, "authfail", "")
			return
		}
		sc.authed = true
		return
	}
	switch action {
	case "settimeout":
		timeout, _ := strconv.Atoi(content)
		log.Println("set timeout", timeout)
	case "authfail":
		fmt.Println("auth key not eq")
		sc.Quit()
	case "tunnel_error":
		if conn != nil {
			conn.Write([]byte(content))
			log.Println("tunnel error", content, sessionId)
		}
		sc.removeSession(sessionId)
	case "showandquit":
		println(content)
		sc.Quit()
	case "tunnel_msg_s":
		if conn != nil {
			conn.Write([]byte(content))
		} else {
			//log.Println("cannot tunnel msg", sessionId)
		}
	case "tunnel_close_s":
		sc.removeSession(sessionId)
	case "init_action_back":
		log.Println("server force do action", content)
		sc.action = content
	case "init_action":
		sc.action = content
		log.Println("init action", content)
		if *remoteAction != "" && *remoteAction != sc.action {
			sc.action = *remoteAction
			go common.WriteCrypt(pipe, sessionId, "init_action_back", *remoteAction, sc.encode)
		}
	case "reverse":
		sc.reverseAddr = content
		go sc.MultiListen()
	case "ready":
		sc.readyId = common.GetId("ready")
		common.WriteCrypt(pipe, "-1", "readyback", sc.readyId, sc.encode)
		time.AfterFunc(time.Minute, func() { common.RmId("ready", sc.readyId) })
	case "readyback":
		go func() {
			for i, conn := range sc.pipes {
				if i > 0 {
					common.Write(conn, "-1", "collect", content)
				} else if i == 0 {
					if *bReverse {
						common.WriteCrypt(conn, "-1", "reverse", *localAddr, sc.encode)
					} else {
						go sc.MultiListen()
					}
				}
			}
		}()
	case "collect":
		readyId := content
		for _, c := range g_ClientMap {
			if c.readyId == readyId {
				log.Println("collect", sc.id, "=>", c.id)
				for i := 1; i < maxPipes; i++ {
					_, b := c.pipes[i]
					if !b {
						c.pipes[i] = pipe
						c.pipesInfo[i] = &pipeInfo{0, 0, 0}
						log.Println("collect", sc.id, "=>", c.id, "pipe", i)
						sc.owner = c
						sc.newindex = i
						break
					}
				}
				break
			}
		}
	case "init_enc":
		tail := common.Xor(content)
		log.Println("got encrpyt key", tail)
		aesKey := "asd4" + tail
		aesBlock, _ := aes.NewCipher([]byte(aesKey))
		sc.SetCrypt(getEncodeFunc(aesBlock), getDecodeFunc(aesBlock))
	case "tunnel_msg_c":
		if conn != nil {
			//log.Println("tunnel", (content), sessionId)
			conn.Write([]byte(content))
		}
	case "tunnel_close":
		sc.removeSession(sessionId)
	case "tunnel_open":
		if sc.action != "socks5" {
			s_conn, err := net.DialTimeout("tcp", sc.action, 10*time.Second)
			if err != nil {
				log.Println("connect to local server fail:", err.Error(), sc.action)
				msg := "cannot connect to bind addr" + sc.action
				go common.WriteCrypt(pipe, sessionId, "tunnel_error", msg, sc.encode)
				return
			} else {
				sc.sessions[sessionId] = &clientSession{pipe: pipe, localConn: s_conn, quit: make(chan bool)}
				go handleLocalPortResponse(sc, sessionId, "")
			}
		} else {
			session = &clientSession{pipe: pipe, localConn: nil, status: "init", recvMsg: "", quit: make(chan bool)}
			sc.sessions[sessionId] = session
			go func() {
				var hello reqMsg
				bOk, _ := hello.read([]byte(content))
				if !bOk {
					msg := "hello read err"
					go common.WriteCrypt(pipe, sessionId, "tunnel_error", msg, sc.encode)
					return
				}
				var ansmsg ansMsg
				url := hello.url
				var s_conn net.Conn
				var err error
				if *dnsCacheNum > 0 && hello.atyp == 3 {
					host := string(hello.dst_addr[1 : 1+hello.dst_addr[0]])
					resChan := make(chan *dnsQueryRes)
					debug("try cache", resChan)
					checkDns <- &dnsQueryReq{c: resChan, host: host, port: int(hello.dst_port2), reqtype: hello.reqtype, url: url}
					debug("try cache2")
					res := <-resChan
					debug("try cache3")
					s_conn = res.conn
					err = res.err
					if res.ip != "" {
						url = res.ip + fmt.Sprintf(":%d", hello.dst_port2)
					}
				}
				if s_conn == nil && err == nil {
					//log.Println("try dial", url)
					s_conn, err = net.DialTimeout(hello.reqtype, url, 30*time.Second)
					//log.Println("try dial", url, "ok")
				}
				if err != nil {
					log.Println("connect to local server fail:", err.Error())
					ansmsg.gen(&hello, 4)
					go common.WriteCrypt(pipe, sessionId, "tunnel_msg_s", string(ansmsg.buf[:ansmsg.mlen]), sc.encode)
				} else {
					session.localConn = s_conn
					go handleLocalPortResponse(sc, sessionId, hello.url)
					ansmsg.gen(&hello, 0)
					go common.WriteCrypt(pipe, sessionId, "tunnel_msg_s", string(ansmsg.buf[:ansmsg.mlen]), sc.encode)
				}
			}()
		}
	}
}

func (sc *Client) SetCrypt(encode, decode func([]byte) []byte) {
	sc.encode = encode
	sc.decode = decode
}

func (sc *Client) Quit() {
	if sc.quit == nil {
		return
	}
	close(sc.quit)
	sc.quit = nil
	log.Println("client quit", sc.id)
	delete(g_ClientMap, sc.id)
	for id, _ := range sc.sessions {
		sc.removeSession(id)
	}
	for id, pipe := range sc.pipes {
		pipe.Close()
		delete(sc.pipes, id)
		delete(sc.pipesInfo, id)
	}
	if sc.listener != nil {
		sc.listener.Close()
	}
}

///////////////////////multi pipe support
var g_LocalConn net.Listener

func (sc *Client) MultiListen() bool {
	if sc.listener == nil {
		var err error
		sc.listener, err = net.Listen("tcp", sc.reverseAddr)
		if err != nil {
			log.Println("cannot listen addr:" + err.Error())
			for _, pipe := range sc.pipes {
				common.WriteCrypt(pipe, "-1", "showandquit", "cannot listen addr:"+err.Error(), sc.encode)
			}
			return false
		}
		println("client service start success,please connect", sc.reverseAddr)
		func() {
			for {
				conn, err := sc.listener.Accept()
				if err != nil {
					break
				}
				sessionId := common.GetId("udp")
				pipe := sc.getOnePipe()
				if pipe == nil {
					log.Println("cannot get pipe for client, wait for recover...")
					time.Sleep(time.Second)
					continue
				}
				sc.sessions[sessionId] = &clientSession{pipe: pipe, localConn: conn, status: "init"}
				//log.Println("client", sc.id, "create session", sessionId)
				go handleLocalServerResponse(sc, sessionId)
			}
			sc.listener = nil
			for _, pipe := range sc.pipes {
				common.WriteCrypt(pipe, "-1", "showandquit", "server lisnter quit", sc.encode)
			}
		}()
	}
	return true
}

func (sc *Client) getOnePipe() net.Conn {
	size := len(sc.pipes)
	if size == 1 {
		pipe, b := sc.pipes[0]
		if b {
			return pipe
		}
	}
	//tmp := []int{}
	choose := 0
	min := -1
	now := time.Now().Unix()
	for id, _ := range sc.pipes {
		//tmp = append(tmp, id)
		info, _ := sc.pipesInfo[id]
		rate := info.bytes
		if now-info.t > 60 {
			rate = 0
		} else {
			if info.times > 1 {
				rate /= info.times
			}
		}
		if min == -1 {
			min = rate
			choose = id
		} else if rate < min {
			min = rate
			choose = id
		}
	}
	//log.Println("choose pipe for ", choose, "of", size, min)
	pipe, _ := sc.pipes[choose]
	return pipe
}

///////////////////////multi pipe support
func handleLocalPortResponse(client *Client, id, url string) {
	sessionId := id
	session := client.getSession(sessionId)
	if session == nil {
		return
	}
	conn := session.localConn
	if conn == nil {
		return
	}
	go func() {
		t := time.NewTicker(time.Minute * 5)
	out:
		for {
			select {
			case <-t.C:
				client.removeSession(id)
				break out
			case <-session.quit:
				break out
			}
		}
		t.Stop()
	}()
	arr := make([]byte, pipe.WriteBufferSize)
	debug("@@@@@@@ debug begin", url)
	reader := bufio.NewReader(conn)
	for {
		size, err := reader.Read(arr)
		if err != nil {
			break
		}
		debug("====debug read", size, url)
		if common.WriteCrypt(session.pipe, id, "tunnel_msg_s", string(arr[0:size]), client.encode) != nil {
			break
		}
		debug("!!!!debug write", size, url)
	}
	// log.Println("handlerlocal down")
	if client.removeSession(sessionId) {
		common.WriteCrypt(session.pipe, id, "tunnel_close_s", "", client.encode)
	}
}

func handleLocalServerResponse(client *Client, sessionId string) {
	session := client.getSession(sessionId)
	if session == nil {
		return
	}
	buffSize := pipe.WriteBufferSize
	pipe := session.pipe
	if pipe == nil {
		return
	}
	conn := session.localConn
	if client.action != "socks5" {
		common.WriteCrypt(pipe, sessionId, "tunnel_open", "", client.encode)
	}
	arr := make([]byte, buffSize)
	reader := bufio.NewReader(conn)
	bParsed := false
	bNeedBreak := false
	for {
		size, err := reader.Read(arr)
		if err != nil {
			break
		}
		if client.action == "socks5" && !bParsed {
			session.processSockProxy(sessionId, string(arr[0:size]), func(head []byte) {
				common.WriteCrypt(pipe, sessionId, "tunnel_open", string(head), client.encode)
				if common.WriteCrypt(pipe, sessionId, "tunnel_msg_c", session.recvMsg, client.encode) != nil {
					bNeedBreak = true
				}
				bParsed = true
			})
		} else {
			if common.WriteCrypt(pipe, sessionId, "tunnel_msg_c", string(arr[0:size]), client.encode) != nil {
				bNeedBreak = true
			}
		}
		if bNeedBreak {
			break
		}
	}
	common.WriteCrypt(pipe, sessionId, "tunnel_close", "", client.encode)
	client.removeSession(sessionId)
}
