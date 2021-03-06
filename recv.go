// RTLAMR - An rtl-sdr receiver for smart meters operating in the 900MHz ISM band.
// Copyright (C) 2015 Douglas Hall
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"runtime/pprof"
	"strings"
	"sync"
	"time"

	"github.com/bemasher/rtlamr/idm"
	"github.com/bemasher/rtlamr/parse"
	"github.com/bemasher/rtlamr/r900"
	"github.com/bemasher/rtlamr/scm"
	"github.com/bemasher/rtltcp"
)

var rcvr Receiver

type Receiver struct {
	rtltcp.SDR
	p  parse.Parser
	q  parse.Parser
	fc parse.FilterChain
}

func (rcvr *Receiver) NewReceiver() {
	switch strings.ToLower(*msgType) {
	case "scm":
		rcvr.p = scm.NewParser(*symbolLength, *decimation)
	case "idm":
		rcvr.p = idm.NewParser(*symbolLength, *decimation)
	case "scm+idm":
		rcvr.p = idm.NewParser(*symbolLength, *decimation)
		rcvr.q = scm.NewParser(*symbolLength, *decimation)
	case "r900":
		rcvr.p = r900.NewParser(*symbolLength, *decimation)
	default:
		log.Fatalf("Invalid message type: %q\n", *msgType)
	}

	if !*quiet {
		rcvr.p.Log()
		if(rcvr.q != nil) {
			rcvr.q.Log()
		}
	}

	// Connect to rtl_tcp server.
	if err := rcvr.Connect(nil); err != nil {
		log.Fatal(err)
	}

	rcvr.HandleFlags()

	// Tell the user how many gain settings were reported by rtl_tcp.
	if !*quiet {
		log.Println("GainCount:", rcvr.SDR.Info.GainCount)
	}

	centerfreqFlagSet := false
	sampleRateFlagSet := false
	gainFlagSet := false
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "centerfreq":
			centerfreqFlagSet = true
		case "samplerate":
			sampleRateFlagSet = true
		case "gainbyindex", "tunergainmode", "tunergain", "agcmode":
			gainFlagSet = true
		case "unique":
			rcvr.fc.Add(NewUniqueFilter())
		case "filterid":
			rcvr.fc.Add(meterID)
		case "filtertype":
			rcvr.fc.Add(meterType)
		}
	})

	// Set some parameters for listening.
	if centerfreqFlagSet {
		rcvr.SetCenterFreq(uint32(rcvr.Flags.CenterFreq))
	} else {
		rcvr.SetCenterFreq(rcvr.p.Cfg().CenterFreq)
	}

	if !sampleRateFlagSet {
		rcvr.SetSampleRate(uint32(rcvr.p.Cfg().SampleRate))
	}
	if !gainFlagSet {
		rcvr.SetGainMode(true)
	}

	return
}

func (rcvr *Receiver) Run() {
	in, out := io.Pipe()
	in2, out2 := io.Pipe()

	go func() {
		tcpBlock := make([]byte, 16384)
		for {
			n, err := rcvr.Read(tcpBlock)
			if err != nil {
				return
			}
			out.Write(tcpBlock[:n])
			if(rcvr.q != nil) {
				out2.Write(tcpBlock[:n])
			}
		}
	}()

	var wg sync.WaitGroup
	start := time.Now()
	looper := func(p parse.Parser, fc parse.FilterChain, in io.Reader) {
		// Setup signal channel for interruption.
		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, os.Kill, os.Interrupt)

		// Setup time limit channel
		tLimit := make(<-chan time.Time, 1)
		if *timeLimit != 0 {
			tLimit = time.After(*timeLimit)
		}

		block := make([]byte, p.Cfg().BlockSize2)
		for {
			// Exit on interrupt or time limit, otherwise receive.
			select {
			case <-sigint:
				return
			case <-tLimit:
				fmt.Println("Time Limit Reached:", time.Since(start))
				return
			default:
				// Read new sample block.
				_, err := io.ReadFull(in, block)
				if err != nil {
					log.Fatal("Error reading samples: ", err)
				}

				pktFound := false
				indices := p.Dec().Decode(block)

				for _, pkt := range p.Parse(indices) {
					if !fc.Match(pkt) {
						continue
					}

					var msg parse.LogMessage
					msg.Time = time.Now()
					msg.Offset, _ = sampleFile.Seek(0, os.SEEK_CUR)
					msg.Length = p.Cfg().BufferLength << 1
					msg.Message = pkt

					err = encoder.Encode(msg)
					if err != nil {
						log.Fatal("Error encoding message: ", err)
					}

					// The XML encoder doesn't write new lines after each
					// element, add them.
					if _, ok := encoder.(*xml.Encoder); ok {
						fmt.Fprintln(logFile)
					}

					pktFound = true
					if *single {
						if len(meterID.UintMap) == 0 {
							break
						} else {
							delete(meterID.UintMap, uint(pkt.MeterID()))
						}
					}
				}

				if pktFound {
					if *sampleFilename != os.DevNull {
						_, err = sampleFile.Write(p.Dec().IQ)
						if err != nil {
							log.Fatal("Error writing raw samples to file:", err)
						}
					}
					if *single && len(meterID.UintMap) == 0 {
						return
					}
				}
			}
		}
	}

	wg.Add(1);
	go looper(rcvr.p, rcvr.fc, in);
	if(rcvr.q != nil) {
		wg.Add(1);
		go looper(rcvr.q, rcvr.fc, in2);
	}

	wg.Wait();

}

func init() {
	log.SetFlags(log.Lshortfile | log.Lmicroseconds)
}

var cpuprofile = flag.String("cpuprofile", "", "write cpu profile to this file")

func main() {
	rcvr.RegisterFlags()
	RegisterFlags()

	flag.Parse()
	HandleFlags()

	rcvr.NewReceiver()

	defer logFile.Close()
	defer sampleFile.Close()
	defer rcvr.Close()

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	rcvr.Run()
}
