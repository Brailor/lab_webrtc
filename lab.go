package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/ivfreader"
	"github.com/pion/webrtc/v3/pkg/media/oggreader"
)

const (
	videoFileName   = "videos/output.ivf"
	audioFileName   = "videos/output.ogg"
	oggPageDuration = time.Millisecond * 20
)


var files = []string{
	"./pages/home.html",
	"./pages/video.html",
	"./templates/base.tmpl.html",
}
var templates = template.Must(template.ParseFiles(files...))

type Page struct {
	Title  string
	Videos []Video
}
type Video struct {
	Name string
}

var peerConnection *webrtc.PeerConnection

func readVideoByChunk(source string, conn *websocket.Conn) {
	file, err := os.Open(source)

	if err != nil {
		log.Fatalf("Error to read [file=%v]: %v", source, err.Error())
	}

	chunk := make([]byte, 0, 1000*1024)
	n_bytes, n_chunks := int64(0), int64(0)

	for {
		read_bytes, err := file.Read(chunk[:cap(chunk)])
		chunk = chunk[:read_bytes]

		if read_bytes == 0 {
			if err == nil {
				continue
			}
			if err == io.EOF {
				break
			}
			log.Fatal(err)
		}

		if err = conn.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
			log.Fatal(err)
		}

		n_chunks++
		n_bytes += int64(read_bytes)

		if err != nil && err != io.EOF {
			log.Fatal(err)
		}
	}

	fmt.Println("total bytes: ", n_bytes, "total chunks: ", n_chunks)
}

func renderTemplate(writer http.ResponseWriter, temp string, page *Page) {
	err := templates.ExecuteTemplate(writer, temp, page)

	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
	}
}

func streamMedia(peerConnection webrtc.PeerConnection) {
	// Create a video track
	videoTrack, videoTrackErr := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, "video", "pion")
	if videoTrackErr != nil {
		panic(videoTrackErr)
	}

	rtpSender, videoTrackErr := peerConnection.AddTrack(videoTrack)
	if videoTrackErr != nil {
		panic(videoTrackErr)
	}

	// Read incoming RTCP packets
	// Before these packets are returned they are processed by interceptors. For things
	// like NACK this needs to be called.
	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
				return
			}
		}
	}()

	go func() {
		// Open a IVF file and start reading using our IVFReader
		file, ivfErr := os.Open(videoFileName)
		if ivfErr != nil {
			panic(ivfErr)
		}

		ivf, header, ivfErr := ivfreader.NewWith(file)
		if ivfErr != nil {
			panic(ivfErr)
		}

		// Wait for connection established
		// <-iceConnectedCtx.Done()

		// Send our video file frame at a time. Pace our sending so we send it at the same speed it should be played back as.
		// This isn't required since the video is timestamped, but we will such much higher loss if we send all at once.
		//
		// It is important to use a time.Ticker instead of time.Sleep because
		// * avoids accumulating skew, just calling time.Sleep didn't compensate for the time spent parsing the data
		// * works around latency issues with Sleep (see https://github.com/golang/go/issues/44343)
		ticker := time.NewTicker(time.Millisecond * time.Duration((float32(header.TimebaseNumerator)/float32(header.TimebaseDenominator))*1000))
		for ; true; <-ticker.C {
			frame, _, ivfErr := ivf.ParseNextFrame()
			if ivfErr == io.EOF {
				fmt.Printf("All video frames parsed and sent")
				// os.Exit(0)
				break
			}

			if ivfErr != nil {
				panic(ivfErr)
			}

			if ivfErr = videoTrack.WriteSample(media.Sample{Data: frame, Duration: time.Second}); ivfErr != nil {
				panic(ivfErr)
			}
		}
	}()

	// Create a audio track
	audioTrack, audioTrackErr := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion")
	if audioTrackErr != nil {
		panic(audioTrackErr)
	}

	rtpaSender, audioTrackErr := peerConnection.AddTrack(audioTrack)
	if audioTrackErr != nil {
		panic(audioTrackErr)
	}

	// Read incoming RTCP packets
	// Before these packets are returned they are processed by interceptors. For things
	// like NACK this needs to be called.
	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := rtpaSender.Read(rtcpBuf); rtcpErr != nil {
				return
			}
		}
	}()

	go func() {
		// Open a OGG file and start reading using our OGGReader
		file, oggErr := os.Open(audioFileName)
		if oggErr != nil {
			panic(oggErr)
		}

		// Open on oggfile in non-checksum mode.
		ogg, _, oggErr := oggreader.NewWith(file)
		if oggErr != nil {
			panic(oggErr)
		}

		// Wait for connection established

		// Keep track of last granule, the difference is the amount of samples in the buffer
		var lastGranule uint64

		// It is important to use a time.Ticker instead of time.Sleep because
		// * avoids accumulating skew, just calling time.Sleep didn't compensate for the time spent parsing the data
		// * works around latency issues with Sleep (see https://github.com/golang/go/issues/44343)
		ticker := time.NewTicker(oggPageDuration)
		for ; true; <-ticker.C {
			pageData, pageHeader, oggErr := ogg.ParseNextPage()
			if oggErr == io.EOF {
				fmt.Printf("All audio pages parsed and sent")
				// os.Exit(0)
				break
			}

			if oggErr != nil {
				panic(oggErr)
			}

			// The amount of samples is the difference between the last and current timestamp
			sampleCount := float64(pageHeader.GranulePosition - lastGranule)
			lastGranule = pageHeader.GranulePosition
			sampleDuration := time.Duration((sampleCount/48000)*1000) * time.Millisecond

			if oggErr = audioTrack.WriteSample(media.Sample{Data: pageData, Duration: sampleDuration}); oggErr != nil {
				panic(oggErr)
			}
		}
	
	}()
}

func signal(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
		if err != nil {
			log.Fatal(err)
		}
		
		var offer webrtc.SessionDescription
		 
		err = json.Unmarshal(body, &offer)
		if err != nil {
			log.Panic(err)
		}

		if err = peerConnection.SetRemoteDescription(offer); err != nil {
			log.Panic(err)
		}

		gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

		answer, err := peerConnection.CreateAnswer(nil)

		if err != nil {
			log.Panic(err)
		}

		if err = peerConnection.SetLocalDescription(answer); err != nil {
			log.Panic(err)
		}
		

		<- gatherComplete
		
		response, err := json.Marshal(*peerConnection.LocalDescription())
		if err != nil {
			log.Panic(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(response)
}
func main() {
	// Create a new RTCPeerConnection
	var err error

	if peerConnection, err = webrtc.NewPeerConnection(webrtc.Configuration{}); err != nil {
		panic(err)
	}
	defer func() {
		if cErr := peerConnection.Close(); cErr != nil {
			fmt.Printf("cannot close peerConnection: %v\n", cErr)
		}
	}()

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("Connection State has changed %s \n", connectionState.String())
		if connectionState == webrtc.ICEConnectionState(webrtc.PeerConnectionStateFailed) {
			fmt.Printf("ICE Connection state changed: %s\n", connectionState.String())
			//os.Exit(0)
		}
	})

	// Set the handler for Peer connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		fmt.Printf("Peer Connection State has changed: %s\n", state.String())
		if state == webrtc.PeerConnectionStateFailed {
			// Wait until PeerConnection has had no network activity for 30 seconds or another failure. It may be reconnected using an ICE Restart.
			// Use webrtc.PeerConnectionStateDisconnected if you are interested in detecting faster timeout.
			// Note that the PeerConnection may come back from PeerConnectionStateDisconnected.
			fmt.Println("Peer Connection has gone to failed exiting")
			os.Exit(0)
		}
	})


	http.HandleFunc("/video/", func(w http.ResponseWriter, r *http.Request) {
		url, _ := r.URL.Parse(r.URL.RawQuery)
		fmt.Println(url)
		// Assert that we have an audio or video file
		// TODO: get video name from request
		_, err := os.Stat(videoFileName)
		haveVideoFile := !os.IsNotExist(err)

		_, err = os.Stat(audioFileName)
		haveAudioFile := !os.IsNotExist(err)

		if !haveAudioFile && !haveVideoFile {
			panic("Could not find `" + audioFileName + "` or `" + videoFileName + "`")
		}

		go streamMedia(*peerConnection)
		signal(w, r)
	})

	http.HandleFunc("/signal", signal)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		dir_entry, err := os.ReadDir("videos")

		if err != nil {
			fmt.Println("error, no videos found under <videos> directory")
			return
		}
		var videos []Video
		for _, v := range dir_entry {
			videos = append(videos, Video{Name: v.Name()})
		}

		renderTemplate(w, "home.html", &Page{Title: "Videos", Videos: videos})
	})

	http.ListenAndServe(":8082", nil)
}
