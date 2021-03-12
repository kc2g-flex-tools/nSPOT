package main

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kc2g-flex-tools/flexclient"
	"github.com/rs/zerolog"
	log "github.com/rs/zerolog/log"
)

var cfg struct {
	RadioIP       string
	Station       string
	Callsign      string
	ClusterServer string
	Timeout       time.Duration
	Filter        string
}

func init() {
	flag.StringVar(&cfg.RadioIP, "radio", ":discover:", "radio IP address or discovery spec")
	flag.StringVar(&cfg.Station, "station", "Flex", "station name to bind to")
	flag.StringVar(&cfg.Callsign, "callsign", "", "callsign for login")
	flag.StringVar(&cfg.ClusterServer, "server", "", "cluster server to connect to")
	flag.DurationVar(&cfg.Timeout, "timeout", 5*time.Minute, "spot persistence timeout")
	flag.StringVar(&cfg.Filter, "filter", "", "spot filter")
}

func main() {
	spotPattern := regexp.MustCompile(`^DX de (\S+?):\s+([0-9.]+)\s+(\S+?)\s+(.*?)\s*[0-9]{4}Z`)

	log.Logger = zerolog.New(
		zerolog.ConsoleWriter{
			Out: os.Stderr,
		},
	).With().Timestamp().Logger()

	flag.Parse()
	if cfg.Callsign == "" {
		flag.Usage()
		log.Fatal().Msg("-callsign is required")
	}

	if cfg.ClusterServer == "" {
		flag.Usage()
		log.Fatal().Msg("-server is required")
	}

	fc, err := flexclient.NewFlexClient(cfg.RadioIP)
	if err != nil {
		log.Fatal().Err(err).Send()
	}

	tc, err := net.Dial("tcp", cfg.ClusterServer)
	if err != nil {
		log.Fatal().Err(err).Send()
	}

	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		<-c
		log.Info().Msg("Exit on SIGINT")
		fc.Close()
		tc.Close()
	}()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		lifetimeSecs := cfg.Timeout / time.Second

		lines := bufio.NewScanner(tc)
		for lines.Scan() {
			line := lines.Text()
			if m := spotPattern.FindStringSubmatch(line); m != nil {
				spotCall, freq, dxCall, comment := m[1], m[2], m[3], m[4]
				freqKhz, err := strconv.ParseFloat(freq, 64)
				if err != nil {
					log.Error().Err(err).Send()
					continue
				}
				log.Info().Str("spotter", spotCall).Str("freq", freq).Str("dx", dxCall).Str("comment", comment).Msg("spot")
				strings.ReplaceAll(spotCall, " ", "\x7f")
				strings.ReplaceAll(freq, " ", "\x7f")
				strings.ReplaceAll(dxCall, " ", "\x7f")
				strings.ReplaceAll(comment, " ", "\x7f")
				res := fc.SendAndWait(fmt.Sprintf("spot add rx_freq=%f callsign=%s spotter_callsign=%s comment=%s lifetime_seconds=%d", freqKhz/1000.0, dxCall, spotCall, comment, lifetimeSecs))
				if res.Error != 0 {
					log.Error().Uint32("error", res.Error).Msg(res.Message)
				}
			} else {
				log.Info().Msg(line)
			}
		}
		fc.Close()
		wg.Done()
	}()

	go func() {
		fc.Run()
		tc.Close()
		wg.Done()
	}()

	fmt.Fprintf(tc, "%s\n", cfg.Callsign)
	fmt.Fprintf(tc, "set dx filter %s\n", cfg.Filter)

	wg.Wait()
}
