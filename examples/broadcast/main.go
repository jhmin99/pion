// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

//go:build !js
// +build !js

// broadcast demonstrates how to broadcast a video to many peers, while only requiring the broadcaster to upload once.
package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/intervalpli"
	"github.com/pion/webrtc/v4"
)

// 명령줄 인수 처리 및 HTTP 서버 설정
// WebRTC Peer Connection 설정
// 미디어 트랙 관리 및 중계
// 브로드캐스터와 피어 간의 연결 관리
// SDP(Signaling Description Protocol) 인코딩/디코딩
// HTTP 서버를 통한 SDP 메시지 처리
func main() { // nolint:gocognit

	// flag 패키지는 Go에서 명령줄 인수를 쉽게 처리할 수 있도록 도와주는 표준 라이브러리입니다.
	// "port": 플래그의 이름입니다. 사용자가 명령줄에서 -port 옵션을 사용할 때 이 이름을 참조합니다.
	// 8080: 플래그의 기본값입니다. 사용자가 명령줄에서 -port 옵션을 지정하지 않으면 기본값으로 8080이 사용됩니다.
	// "http server port": 플래그에 대한 설명입니다. 이 설명은 -help 옵션을 사용할 때 표시됩니다.
	// flag.Int는 포인터(*int)를 반환합니다. 이 포인터는 플래그의 값을 저장하는 변수입니다.
	port := flag.Int("port", 8080, "http server port")

	// flag.Parse() 함수는 명령줄 인수를 실제로 파싱(parse)하여 정의된 플래그 변수에 값을 할당합니다.
	// 이 함수가 호출되기 전까지는 플래그 변수에 값이 할당되지 않습니다. 따라서, 플래그를 정의한 후 반드시 flag.Parse()를 호출하여 플래그의 값을 실제로 읽어와야 합니다.
	flag.Parse()

	// httpSDPServer 함수 호출
	// HTTP 서버를 시작하고, SDP 메시지를 수신할 수 있는 채널(sdpChan)을 반환받습니다.
	// *port는 앞서 파싱한 포트 번호입니다.
	sdpChan := httpSDPServer(*port)

	//webrtc.SessionDescription 객체: SDP Offer를 저장하기 위한 객체입니다.
	offer := webrtc.SessionDescription{}
	// decode 함수: Base64로 인코딩된 SDP 문자열을 디코딩하여 offer 객체에 할당합니다.
	// <-sdpChan: sdpChan 채널에서 SDP Offer를 수신합니다.
	// 이는 HTTP 서버로부터 브라우저가 전송한 SDP Offer를 기다리는 부분입니다.
	decode(<-sdpChan, &offer)
	fmt.Println("")

	// webrtc.Configuration: WebRTC Peer Connection 설정을 정의합니다.
	peerConnectionConfig := webrtc.Configuration{
		// ICE 서버 설정: NAT Traversal을 지원하기 위해 STUN 서버를 설정합니다. 여기서는 Google의 STUN 서버(stun.l.google.com:19302)를 사용합니다.
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}
	// webrtc.MediaEngine: 미디어 트랙의 인코딩 및 디코딩을 관리합니다.
	m := &webrtc.MediaEngine{}
	// RegisterDefaultCodecs 함수: 기본 코덱을 등록합니다. 이는 WebRTC가 지원하는 기본적인 오디오 및 비디오 코덱을 설정합니다.
	if err := m.RegisterDefaultCodecs(); err != nil {
		panic(err)
	}

	// interceptor.Registry:
	// RTP/RTCP 파이프라인을 설정하여 다양한 네트워크 기능(NACKs, RTCP Reports 등)을 제공합니다.
	// WebRTC Peer Connection 생성 시 기본적으로 인터셉터가 활성화되지만, 수동으로 관리하는 경우 interceptor.Registry를 생성해야 합니다.
	i := &interceptor.Registry{}

	// RegisterDefaultInterceptors 함수: 기본적인 인터셉터를 Media Engine과 Registry에 등록합니다. 이는 기본적인 RTP/RTCP 기능을 제공합니다.
	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		panic(err)
	}

	// intervalpli.NewReceiverInterceptor 함수:
	// 일정 간격(기본 3초)으로 PLI(Picture Loss Indication)를 전송하는 인터셉터를 생성합니다.
	// PLI의 역할:
	// 비디오 키프레임을 강제 생성하여 스트림의 안정성과 오류 복원력을 향상시킵니다.
	// 그러나, 이로 인해 비디오 품질이 저하되고 비트레이트가 증가할 수 있습니다.
	// 이유 - bear 참고 (pion-example/broadcast/키프레임 강제생성 이유)
	intervalPliFactory, err := intervalpli.NewReceiverInterceptor()
	if err != nil {
		panic(err)
	}
	// 생성한 intervalPliFactory를 interceptor.Registry에 추가합니다.
	i.Add(intervalPliFactory)

	// Create a new RTCPeerConnection
	// webrtc.NewAPI를 사용하여 Media Engine과 Interceptor Registry를 포함한 새로운 WebRTC API를 생성합니다.
	// NewPeerConnection 함수는 설정한 peerConnectionConfig를 사용하여 새로운 Peer Connection을 생성합니다.
	// 에러 처리: Peer Connection 생성에 실패하면 패닉을 발생시킵니다.
	// defer 문: 프로그램 종료 시 Peer Connection을 안전하게 닫습니다.
	peerConnection, err := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(i)).NewPeerConnection(peerConnectionConfig)
	if err != nil {
		panic(err)
	}
	defer func() {
		if cErr := peerConnection.Close(); cErr != nil {
			fmt.Printf("cannot close peerConnection: %v\n", cErr)
		}
	}()

	// Allow us to receive 1 video track
	// Transceiver 추가: RTP 스트림을 송수신할 수 있는 단위입니다.
	// AddTransceiverFromKind 함수는 비디오 트랙을 수신할 수 있도록 트랜시버를 추가합니다.
	// RTPCodecTypeVideo: 비디오 코덱을 사용하는 RTP 스트림을 수신하도록 설정합니다.
	if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

	// localTrackChan 채널: 로컬 트랙(TrackLocalStaticRTP)을 전달하기 위한 채널입니다.
	localTrackChan := make(chan *webrtc.TrackLocalStaticRTP)
	// peerConnection.OnTrack 핸들러:
	// 원격 피어로부터 새로운 트랙이 시작될 때 호출되는 핸들러입니다.
	// remoteTrack: 원격 피어로부터 수신된 트랙 객체입니다.
	// receiver: RTP 수신기 객체입니다.
	peerConnection.OnTrack(func(remoteTrack *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) { //nolint: revive
		// Create a local track, all our SFU clients will be fed via this track
		// NewTrackLocalStaticRTP 함수:
		// 원격 트랙의 코덱 정보를 기반으로 새로운 로컬 트랙을 생성합니다.
		// "video"는 미디어 종류를 나타내며, "pion"은 트랙의 ID입니다.
		localTrack, newTrackErr := webrtc.NewTrackLocalStaticRTP(remoteTrack.Codec().RTPCodecCapability, "video", "pion")
		if newTrackErr != nil {
			panic(newTrackErr)
		}
		// 채널에 로컬 트랙 전송: 생성한 로컬 트랙을 localTrackChan 채널에 전송합니다.
		localTrackChan <- localTrack

		// RTP 패킷 읽기 및 쓰기:
		// 원격 트랙에서 RTP 패킷을 읽어 로컬 트랙으로 씁니다.
		// 이를 통해 SFU(Simulcast Forwarding Unit) 역할을 수행하며, 여러 피어에게 데이터를 중계합니다.
		rtpBuf := make([]byte, 1400)
		for {
			i, _, readErr := remoteTrack.Read(rtpBuf)
			if readErr != nil {
				panic(readErr)
			}

			// ErrClosedPipe means we don't have any subscribers, this is ok if no peers have connected yet
			if _, err = localTrack.Write(rtpBuf[:i]); err != nil && !errors.Is(err, io.ErrClosedPipe) {
				panic(err)
			}
		}
	})

	// Set the remote SessionDescription
	// SetRemoteDescription 함수: 수신한 SDP Offer를 Peer Connection에 설정하여 원격 피어와의 연결을 설정합니다.
	err = peerConnection.SetRemoteDescription(offer)
	if err != nil {
		panic(err)
	}

	// Create answer
	// CreateAnswer 함수: SDP Offer에 대한 응답인 SDP Answer를 생성합니다.
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}

	// Create channel that is blocked until ICE Gathering is complete
	// GatheringCompletePromise 함수: ICE Gathering이 완료될 때까지 대기하는 채널을 생성합니다.
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	// Sets the LocalDescription, and starts our UDP listeners
	// SetLocalDescription 함수: 생성한 SDP Answer를 Peer Connection에 설정하여 Local Description을 업데이트하고, UDP 리스너를 시작합니다.
	err = peerConnection.SetLocalDescription(answer)
	if err != nil {
		panic(err)
	}

	// Block until ICE Gathering is complete, disabling trickle ICE
	// we do this because we only can exchange one signaling message
	// in a production application you should exchange ICE Candidates via OnICECandidate

	// 대기: ICE Gathering이 완료될 때까지 블록합니다. 이는 Trickle ICE를 비활성화하여 단일 SDP 메시지 교환만 허용하기 위함입니다.
	// 실제 애플리케이션에서는 ICE Candidates를 OnICECandidate 이벤트를 통해 동적으로 교환하는 것이 좋습니다.
	<-gatherComplete

	// Get the LocalDescription and take it to base64 so we can paste in browser
	// SDP Answer 출력:
	// encode 함수: Local Description을 Base64로 인코딩하여 브라우저에 붙여넣을 수 있는 문자열로 변환합니다.
	// fmt.Println 함수: 인코딩된 SDP Answer를 출력합니다.
	fmt.Println(encode(peerConnection.LocalDescription()))

	// localTrackChan에서 로컬 트랙 수신:
	// localTrackChan 채널에서 로컬 트랙을 수신합니다.
	// 이는 앞서 OnTrack 핸들러에서 생성된 로컬 트랙입니다.
	localTrack := <-localTrackChan

	// 무한 루프 (for):
	// 지속적으로 새로운 피어의 Sendonly 연결을 받아 처리합니다.
	for {
		fmt.Println("")
		// "Curl an base64 SDP to start sendonly peer connection" 메시지를 출력하여 사용자가 SDP Offer를 서버에 전송하도록 유도합니다.
		fmt.Println("Curl an base64 SDP to start sendonly peer connection")

		// sdpChan 채널에서 수신한 Base64 인코딩된 SDP Offer를 디코딩하여 recvOnlyOffer 객체에 할당합니다.
		recvOnlyOffer := webrtc.SessionDescription{}
		decode(<-sdpChan, &recvOnlyOffer)

		// Create a new PeerConnection
		// 기존의 peerConnectionConfig를 사용하여 새로운 Peer Connection을 생성합니다.
		peerConnection, err := webrtc.NewPeerConnection(peerConnectionConfig)
		if err != nil {
			panic(err)
		}
		// AddTrack 함수: 기존의 로컬 트랙(localTrack)을 새로운 Peer Connection에 추가합니다.
		// 이는 브로드캐스터의 트랙을 피어에게 전송하기 위한 설정입니다.
		rtpSender, err := peerConnection.AddTrack(localTrack)
		if err != nil {
			panic(err)
		}

		// Read incoming RTCP packets
		// Before these packets are returned they are processed by interceptors. For things
		// like NACK this needs to be called.

		// RTCP 패킷 읽기:
		// 고루틴 실행: RTCP 패킷을 읽어 인터셉터를 통해 처리합니다.
		// RTCP 패킷: 네트워크 상태를 보고하고, NACK(패킷 손실 방지) 등을 처리하는 패킷입니다.
		go func() {
			rtcpBuf := make([]byte, 1500)
			for {
				if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
					return
				}
			}
		}()

		// Set the remote SessionDescription
		// 수신한 SDP Offer(recvOnlyOffer)를 새로운 Peer Connection에 설정하여 연결을 완료합니다.
		err = peerConnection.SetRemoteDescription(recvOnlyOffer)
		if err != nil {
			panic(err)
		}

		// Create answer
		// 새로운 Peer Connection의 SDP Answer를 생성하고, 이를 설정합니다.
		answer, err := peerConnection.CreateAnswer(nil)
		if err != nil {
			panic(err)
		}

		// Create channel that is blocked until ICE Gathering is complete
		gatherComplete = webrtc.GatheringCompletePromise(peerConnection)

		// Sets the LocalDescription, and starts our UDP listeners
		err = peerConnection.SetLocalDescription(answer)
		if err != nil {
			panic(err)
		}

		// Block until ICE Gathering is complete, disabling trickle ICE
		// we do this because we only can exchange one signaling message
		// in a production application you should exchange ICE Candidates via OnICECandidate

		// ICE Gathering 완료 대기: ICE Gathering이 완료될 때까지 대기합니다.
		<-gatherComplete

		// Get the LocalDescription and take it to base64 so we can paste in browser

		// 새로운 Peer Connection의 SDP Answer를 Base64로 인코딩하여 출력합니다.
		// 브라우저에 이 값을 붙여넣어 피어 연결을 완료합니다.
		fmt.Println(encode(peerConnection.LocalDescription()))
	}
}

// JSON encode + base64 a SessionDescription

// SessionDescription 객체를 JSON으로 마샬링한 후 Base64로 인코딩하여 문자열로 변환합니다.
func encode(obj *webrtc.SessionDescription) string {

	// JSON 마샬링: json.Marshal 함수를 사용하여 SessionDescription 객체를 JSON 형식으로 변환합니다.
	b, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}
	// Base64 인코딩: base64.StdEncoding.EncodeToString 함수를 사용하여 JSON 데이터를 Base64 문자열로 인코딩합니다.
	// 반환: 인코딩된 Base64 문자열을 반환합니다.
	return base64.StdEncoding.EncodeToString(b)
}

// Decode a base64 and unmarshal JSON into a SessionDescription

// Base64로 인코딩된 SDP 문자열을 디코딩하여 SessionDescription 객체로 변환합니다.
func decode(in string, obj *webrtc.SessionDescription) {

	// Base64 디코딩: base64.StdEncoding.DecodeString 함수를 사용하여 인코딩된 문자열을 디코딩하여 바이트 배열로 변환합니다.
	b, err := base64.StdEncoding.DecodeString(in)
	if err != nil {
		panic(err)
	}

	// JSON 언마샬링: json.Unmarshal 함수를 사용하여 바이트 배열을 SessionDescription 객체로 변환합니다.
	if err = json.Unmarshal(b, obj); err != nil {
		panic(err)
	}
}

// httpSDPServer starts a HTTP Server that consumes SDPs

// HTTP 서버를 시작하여 브라우저로부터 SDP 메시지를 수신하고, 이를 처리할 수 있는 채널을 제공합니다.
func httpSDPServer(port int) chan string {

	// sdpChan 채널을 생성하여 SDP 메시지를 전달합니다.
	sdpChan := make(chan string)

	// 모든 요청에 대해 / 경로를 처리하는 핸들러를 설정합니다.
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {

		// 요청 본문을 읽어 body 변수에 저장합니다.
		body, _ := io.ReadAll(r.Body)

		// 클라이언트에게 "done" 응답을 보냅니다.
		fmt.Fprintf(w, "done") //nolint: errcheck

		// sdpChan 채널에 수신한 body를 전송합니다.
		sdpChan <- string(body)
	})

	// 고루틴을 사용하여 HTTP 서버를 비동기로 실행합니다.
	go func() {
		// nolint: gosec
		// ListenAndServe 함수는 지정된 포트에서 HTTP 서버를 시작합니다.
		panic(http.ListenAndServe(":"+strconv.Itoa(port), nil))
	}()

	// SDP 메시지를 전달받을 수 있는 sdpChan 채널을 반환합니다.
	return sdpChan
}
