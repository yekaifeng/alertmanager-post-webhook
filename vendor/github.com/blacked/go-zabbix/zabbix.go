// Package implement zabbix sender protocol for send metrics to zabbix.
package zabbix

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"crypto/tls"
	"crypto/sha1"
	"time"
	"strconv"
	"bytes"
	"strings"
	"os"
)

// Metric class.
type Metric struct {
	Host  string `json:"host"`
	Key   string `json:"key"`
	Value string `json:"value"`
	Clock int64  `json:"clock"`
}

type MailBody struct {
	// 1-"text/plain",2-"text/html"
	ContentType      int    `json:"contentType"`
	ContentBody      string `json:"contentBody"`
}

type MailMessage struct {
	From             string `json:"from"`
	To               []string `json:"to"`
	Cc               []string `json:"cc"`
	Bcc              []string `json:"bcc"`
	Subject          string `json:"subject"`
	Body             MailBody `json:"body"`
	Attach           []string `json:"attach"`
}

type MailMessageType struct {
	MsgType        MailMessage `json:"mail"`
}

type AlertMetric struct {
	Time             string `json:"tm"`
	Event            int    `json:"evt"`
	AlertType        int    `json:"type"`
	MessageType      MailMessageType `json:"msg"`
}

// Metric class constructor.
func NewMetric(host, key, value string, clock ...int64) *Metric {
	m := &Metric{Host: host, Key: key, Value: value}
	// use current time, if `clock` is not specified
	if m.Clock = time.Now().Unix(); len(clock) > 0 {
		m.Clock = int64(clock[0])
	}
	return m
}

// AlertMetric class constructor.
func NewAlertMetric(time string, event int, alerttype int, msgtype MailMessageType) *AlertMetric {
	m := &AlertMetric{Time: time,
		              Event: event,
	                  AlertType: alerttype,
		              MessageType: msgtype}
	return m
}

// Packet class.
type Packet struct {
	Request string    `json:"request"`
	Data    []*Metric `json:"data"`
	Clock   int64     `json:"clock"`
}

type AlertPacket struct{
	Request string    `json:"request"`
	Data    []*AlertMetric `json:"data"`
	Clock   int64     `json:"clock"`
}

// Packet class cunstructor.
func NewPacket(data []*Metric, clock ...int64) *Packet {
	p := &Packet{Request: `sender data`, Data: data}
	// use current time, if `clock` is not specified
	if p.Clock = time.Now().Unix(); len(clock) > 0 {
		p.Clock = int64(clock[0])
	}
	return p
}

// Packet class cunstructor.
func NewAlertPacket(data []*AlertMetric, clock ...int64) *AlertPacket {
	p := &AlertPacket{Request: `ocp alerts`, Data: data}
	// use current time, if `clock` is not specified
	if p.Clock = time.Now().Unix(); len(clock) > 0 {
		p.Clock = int64(clock[0])
	}
	return p
}

// DataLen Packet class method, return 8 bytes with packet length in little endian order.
func (p *Packet) DataLen() []byte {
	dataLen := make([]byte, 8)
	JSONData, _ := json.Marshal(p)
	binary.LittleEndian.PutUint32(dataLen, uint32(len(JSONData)))
	return dataLen
}

// Sender class.
type Sender struct {
	Host string
	Port int
}

// Sender class constructor.
func NewSender(host string, port int) *Sender {
	s := &Sender{Host: host, Port: port}
	return s
}

// Method Sender class, return zabbix header.
func (s *Sender) getHeader() []byte {
	return []byte("ZBXD\x01")
}

// Method Sender class, resolve uri by name:port.
func (s *Sender) getTCPAddr() (iaddr *net.TCPAddr, err error) {
	// format: hostname:port
	addr := fmt.Sprintf("%s:%d", s.Host, s.Port)

	// Resolve hostname:port to ip:port
	iaddr, err = net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		err = fmt.Errorf("Connection failed: %s", err.Error())
		return
	}

	return
}

// Method Sender class, make connection to uri.
func (s *Sender) connect() (conn *net.TCPConn, err error) {

	type DialResp struct {
		Conn  *net.TCPConn
		Error error
	}

	// Open connection to zabbix host
	iaddr, err := s.getTCPAddr()
	if err != nil {
		return
	}

	// dial tcp and handle timeouts
	ch := make(chan DialResp)

	go func() {
		conn, err = net.DialTCP("tcp", nil, iaddr)
		ch <- DialResp{Conn: conn, Error: err}
	}()

	select {
	case <-time.After(5 * time.Second):
		err = fmt.Errorf("Connection Timeout")
	case resp := <-ch:
		if resp.Error != nil {
			err = resp.Error
			break
		}

		conn = resp.Conn
	}

	return
}

// Method Sender class, read data from connection.
func (s *Sender) read(conn *net.TCPConn) (res []byte, err error) {
	res = make([]byte, 1024)
	res, err = ioutil.ReadAll(conn)
	if err != nil {
		err = fmt.Errorf("Error whule receiving the data: %s", err.Error())
		return
	}

	return
}

// Method Sender class, send packet to zabbix.
func (s *Sender) Send(packet *Packet) (res []byte, err error) {
	conn, err := s.connect()
	if err != nil {
		return
	}
	defer conn.Close()

	dataPacket, _ := json.Marshal(packet)

	/*
	   fmt.Printf("HEADER: % x (%s)\n", s.getHeader(), s.getHeader())
	   fmt.Printf("DATALEN: % x, %d byte\n", packet.DataLen(), len(packet.DataLen()))
	   fmt.Printf("BODY: %s\n", string(dataPacket))
	*/

	// Fill buffer
	buffer := append(s.getHeader(), packet.DataLen()...)
	buffer = append(buffer, dataPacket...)

	// Sent packet to zabbix
	_, err = conn.Write(buffer)
	if err != nil {
		err = fmt.Errorf("Error while sending the data: %s", err.Error())
		return
	}

	res, err = s.read(conn)

	/*
	   fmt.Printf("RESPONSE: %s\n", string(res))
	*/
	return
}

// Method Sender class, send packet to zabbix.
func (s *Sender) AlertSend(packet *AlertPacket, subpath string) (res []byte, err error) {

	dataPacket, _ := json.Marshal(packet)

	// Get ip and port to zabbix host
	iaddr, err := s.getTCPAddr()
	if err != nil {
		return
	}

	// Set https trasport ignore certificate verification
	tp := &http.Transport{
		TLSClientConfig:    &tls.Config{InsecureSkipVerify: true},
	}
	// New https client for Post
	client := &http.Client{Transport: tp}
	url := "https://" + iaddr.IP.String() + ":" + strconv.Itoa(iaddr.Port) + subpath
	reqest, err := http.NewRequest("POST", url, bytes.NewReader(dataPacket))
	if err != nil {
		fmt.Println("Fatal error ", err.Error())
	}
	//Set request header
	reqest.Header.Set("Content-Type", "application/json")

	//Send request
	resp, err := client.Do(reqest)
	defer resp.Body.Close()

	content, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println("Fatal error ", err.Error())
	}
	fmt.Printf("response: %s:", string(content))

	return
}

// Method Sender class, send packet to zabbix.
func (s *Sender) AlertMetricSend(metric *AlertMetric, subpath string, verifycode string) (res []byte, err error) {

	dataPacket, _ := json.Marshal(metric)

	// Get ip and port to zabbix host
	iaddr, err := s.getTCPAddr()
	if err != nil {
		return
	}

	// Set https trasport ignore certificate verification
	tp := &http.Transport{
		TLSClientConfig:    &tls.Config{InsecureSkipVerify: true},
	}
	// New https client for Post
	client := &http.Client{Transport: tp}
	url := "https://" + iaddr.IP.String() + ":" + strconv.Itoa(iaddr.Port) + subpath
	reqest, err := http.NewRequest("POST", url, bytes.NewReader(dataPacket))
	if err != nil {
		fmt.Println("Fatal error ", err.Error())
	}
	//Set request header
	appid := strings.Split(verifycode, "_")[0]
	timezone := os.Getenv("TIMEZONE")
    if err != nil {
		timezone := "Asia/Shanghai"
	}
	location, _ := time.LoadLocation(timezone)
	utc_time := strconv.FormatInt(time.Now().In(location).UTC().Unix(), 10)
	vc :=  verifycode + utc_time
	//reqest.Header.Set("Content-Type", "application/json")
	reqest.Header.Set("Authorization", "appId:" + getsha1(vc))
	reqest.Header.Add("t", utc_time)

	//Send request
	resp, err := client.Do(reqest)
	defer resp.Body.Close()

	content, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println("Fatal error ", err.Error())
	}
	fmt.Printf("response: %s:", string(content))

	return
}

func getsha1(str string) (string) {
	h := sha1.New()
	h.Write([]byte(str))
	bs := h.Sum(nil)
	hashsha1 := fmt.Sprintf("%x", bs)
	return hashsha1
}