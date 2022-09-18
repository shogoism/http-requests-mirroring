// Original Copyright 2012 Google, Inc. All rights reserved.
//
// Use of this source code is governed by a BSD-style license
// that can be found in the LICENSE file in the root of the source
// tree.

// Modification Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bufio"
	"bytes"
	crypto_rand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"hash/crc64"
	"io"
	"io/ioutil"
	"log"
	math_rand "math/rand"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/examples/util"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
	"github.com/google/gopacket/tcpassembly"
	"github.com/google/gopacket/tcpassembly/tcpreader"
)

var routeTableJson = flag.String("route-table-json", "", "Map of source ip and destination ip.")
var fwdPerc = flag.Float64("percentage", 100, "Must be between 0 and 100.")
var fwdBy = flag.String("percentage-by", "", "Can be empty. Otherwise, valid values are: header, remoteaddr.")
var fwdHeader = flag.String("percentage-by-header", "", "If percentage-by is header, then specify the header here.")
var reqPort = flag.Int("filter-request-port", 80, "Must be between 0 and 65535.")
var fwdMap map[string]string

// Build a simple HTTP request parser using tcpassembly.StreamFactory and tcpassembly.Stream interfaces

// httpStreamFactory implements tcpassembly.StreamFactory
type httpStreamFactory struct{}

// httpStream will handle the actual decoding of http requests.
type httpStream struct {
	net, transport gopacket.Flow
	r              tcpreader.ReaderStream
}

func (h *httpStreamFactory) New(net, transport gopacket.Flow) tcpassembly.Stream {
	hstream := &httpStream{
		net:       net,
		transport: transport,
		r:         tcpreader.NewReaderStream(),
	}
	go hstream.run() // Important... we must guarantee that data from the reader stream is read.

	// ReaderStream implements tcpassembly.Stream, so we can return a pointer to it.
	return &hstream.r
}

func (h *httpStream) run() {
	buf := bufio.NewReader(&h.r)
	for {
		req, err := http.ReadRequest(buf)
		if err == io.EOF {
			// We must read until we see an EOF... very important!
			return
		} else if err != nil {
			log.Println("Error reading stream", h.net, h.transport, ":", err)
		} else {
			reqSourceIP := h.net.Src().String()
			reqDestionationPort := h.transport.Dst().String()
			body, bErr := ioutil.ReadAll(req.Body)
			if bErr != nil {
				return
			}
			req.Body.Close()
			go forwardRequest(req, reqSourceIP, reqDestionationPort, body)
		}
	}
}

func forwardRequest(req *http.Request, reqSourceIP string, reqDestionationPort string, body []byte) {

	// if percentage flag is not 100, then a percentage of requests is skipped
	if *fwdPerc != 100 {
		var uintForSeed uint64

		if *fwdBy == "" {
			// if percentage-by is empty, then forward only a certain percentage of requests
			var b [8]byte
			_, err := crypto_rand.Read(b[:])
			if err != nil {
				log.Println("Error generating crypto random unit for seed", ":", err)
				return
			}
			// uintForSeed is random
			uintForSeed = binary.LittleEndian.Uint64(b[:])
		} else {
			// if percentage-by is not empty, then forward only requests from a certain percentage of headers/remoteaddresses
			strForSeed := ""
			if *fwdBy == "header" {
				strForSeed = req.Header.Get(*fwdHeader)
			} else {
				strForSeed = reqSourceIP
			}
			crc64Table := crc64.MakeTable(0xC96C5795D7870F42)
			// uintForSeed is derived from strForSeed
			uintForSeed = crc64.Checksum([]byte(strForSeed), crc64Table)
		}

		// generate a consistent random number from the variable uintForSeed
		math_rand.Seed(int64(uintForSeed))
		randomPercent := math_rand.Float64() * 100
		// skip a percentage of requests
		if randomPercent > *fwdPerc {
			return
		}
	}

	// excluding health checker.
	if strings.Contains(req.UserAgent(),"ELB-HealthChecker") {
		return
	}
	// excluding resource files.
	if strings.Contains(req.RequestURI, ".html") {
		return
	}
	if strings.Contains(req.RequestURI, ".js") {
		return
	}
	if strings.Contains(req.RequestURI, ".css") {
		return
	}
	if strings.Contains(req.RequestURI, ".gif") {
		return
	}
	if strings.Contains(req.RequestURI, ".png") {
		return
	}
	if strings.Contains(req.RequestURI, ".jpeg") {
		return
	}
	if strings.Contains(req.RequestURI, ".jpg") {
		return
	}
	if strings.Contains(req.RequestURI, ".svg") {
		return
	}
	if strings.Contains(req.RequestURI, ".webp") {
		return
	}

	// create a new url from the raw RequestURI sent by the client
	if fwdMap[req.Host] == "" {
		fmt.Printf("Request Host "+req.Host+" is not found in augment route-table-json. (%#v)",req)
		return
	}
	url := fmt.Sprintf("%s%s", string(fwdMap[req.Host]), req.RequestURI)
	log.Print(url)

	// create a new HTTP request
	forwardReq, err := http.NewRequest(req.Method, url, bytes.NewReader(body))
	if err != nil {
		return
	}

	// add headers to the new HTTP request
	for header, values := range req.Header {
		for _, value := range values {
			forwardReq.Header.Add(header, value)
		}
	}

	// Append to X-Forwarded-For the IP of the client or the IP of the latest proxy (if any proxies are in between)
	// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/X-Forwarded-For
	forwardReq.Header.Add("X-Forwarded-For", reqSourceIP)
	// The three following headers should contain 1 value only, i.e. the outermost port, protocol, and host
	// https://tools.ietf.org/html/rfc7239#section-5.4
	if forwardReq.Header.Get("X-Forwarded-Port") == "" {
		forwardReq.Header.Set("X-Forwarded-Port", reqDestionationPort)
	}
	if forwardReq.Header.Get("X-Forwarded-Proto") == "" {
		forwardReq.Header.Set("X-Forwarded-Proto", "http")
	}
	if forwardReq.Header.Get("X-Forwarded-Host") == "" {
		forwardReq.Header.Set("X-Forwarded-Host", req.Host)
	}

	// Execute the new HTTP request
	httpClient := &http.Client{}
	resp, rErr := httpClient.Do(forwardReq)
	if rErr != nil {
		// log.Println("Forward request error", ":", err)
		return
	}

	defer resp.Body.Close()
}

// Listen for incoming connections.
func openTCPClient() {
	ln, err := net.Listen("tcp", ":4789")
	if err != nil {
		// If TCP listener cannot be established, NLB health checks would fail
		// For this reason, we OS.exit
		log.Println("Error listening on TCP", ":", err)
		os.Exit(1)
	}
	log.Println("Listening on TCP 4789")
	for {
		// Listen for an incoming connection and close it immediately.
		conn, _ := ln.Accept()
		conn.Close()
	}
}

func main() {
	defer util.Run()()
	var handle *pcap.Handle
	var err error

	flag.Parse()
	//labels validation
	if *fwdPerc > 100 || *fwdPerc < 0 {
		err = fmt.Errorf("Flag percentage is not between 0 and 100. Value: %f.", *fwdPerc)
	} else if *fwdBy != "" && *fwdBy != "header" && *fwdBy != "remoteaddr" {
		err = fmt.Errorf("Flag percentage-by (%s) is not valid.", *fwdBy)
	} else if *fwdBy == "header" && *fwdHeader == "" {
		err = fmt.Errorf("Flag percentage-by is set to header, but percentage-by-header is empty.")
	} else if *reqPort > 65535 || *reqPort < 0 {
		err = fmt.Errorf("Flag filter-request-port is not between 0 and 65535. Value: %f.", *fwdPerc)
	} else {
		err = json.Unmarshal([]byte(*routeTableJson), &fwdMap)
	}
	if err != nil {
		log.Fatal(err)
	}

	// Set up pcap packet capture
	log.Printf("Starting capture on interface vxlan0")
	handle, err = pcap.OpenLive("vxlan0", 8951, true, pcap.BlockForever)
	if err != nil {
		log.Fatal(err)
	}

	// Set up BPF filter
	BPFFilter := fmt.Sprintf("%s%d", "tcp and dst port ", *reqPort)
	if err := handle.SetBPFFilter(BPFFilter); err != nil {
		log.Fatal(err)
	}

	// Set up assembly
	streamFactory := &httpStreamFactory{}
	streamPool := tcpassembly.NewStreamPool(streamFactory)
	assembler := tcpassembly.NewAssembler(streamPool)

	log.Println("reading in packets")
	// Read in packets, pass to assembler.
	packetSource := gopacket.NewPacketSource(handle, handle.LinkType())
	packets := packetSource.Packets()
	ticker := time.Tick(time.Minute)

	//Open a TCP Client, for NLB Health Checks only
	go openTCPClient()

	for {
		select {
		case packet := <-packets:
			// A nil packet indicates the end of a pcap file.
			if packet == nil {
				return
			}
			if packet.NetworkLayer() == nil || packet.TransportLayer() == nil || packet.TransportLayer().LayerType() != layers.LayerTypeTCP {
				log.Println("Unusable packet")
				continue
			}
			tcp := packet.TransportLayer().(*layers.TCP)
			assembler.AssembleWithTimestamp(packet.NetworkLayer().NetworkFlow(), tcp, packet.Metadata().Timestamp)

		case <-ticker:
			// Every minute, flush connections that haven't seen activity in the past 1 minute.
			assembler.FlushOlderThan(time.Now().Add(time.Minute * -1))
		}
	}
}
