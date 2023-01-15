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

	"github.com/chzyer/readline"
	"github.com/gookit/color"
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
}

func init() {
	flag.StringVar(&cfg.RadioIP, "radio", ":discover:", "radio IP address or discovery spec")
	flag.StringVar(&cfg.Station, "station", "Flex", "station name to bind to")
	flag.StringVar(&cfg.Callsign, "callsign", "", "callsign for login")
	flag.StringVar(&cfg.ClusterServer, "server", "", "cluster server to connect to")
	flag.DurationVar(&cfg.Timeout, "timeout", 5*time.Minute, "spot persistence timeout")
}

func main() {
	spotPattern := regexp.MustCompile(`^DX de (\S+?)(:?\s*)([0-9.]+)(\s+)(\S+?)(\s+)(.*?)(\s*)([0-9]{4}Z)`)
	qrtPattern := regexp.MustCompile(`\b(?i:QRT)\b`)

	promptSuffixes := []string{">", "> ", ":", ": "}

	log.Logger = zerolog.New(
		zerolog.ConsoleWriter{
			Out: os.Stderr,
		},
	).With().Timestamp().Logger()

	flag.Parse()
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

	prompt := color.FgLightMagenta.Render("cluster") + "> "
	rl, err := readline.New(prompt)
	if err != nil {
		log.Fatal().Err(err).Msg("creating readline")
	}

	log.Logger = zerolog.New(
		zerolog.ConsoleWriter{
			Out: rl.Stderr(),
		},
	).With().Timestamp().Logger()

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
		lifetimeSecs := int(cfg.Timeout / time.Second)

		lines := bufio.NewScanner(tc)
		for lines.Scan() {
			line := lines.Text()
			if m := spotPattern.FindStringSubmatch(line); m != nil {
				spotCall, freq, dxCall, comment, ts := m[1], m[3], m[5], m[7], m[9]
				freqKhz, err := strconv.ParseFloat(freq, 64)
				if err != nil {
					log.Error().Err(err).Send()
					continue
				}

				var commentColor = color.FgLightCyan
				if qrtPattern.MatchString(comment) {
					// TODO: actually remove the spot. That will need some
					// refactoring to remember the IDs of spots we add, though.
					commentColor = color.FgLightRed
				}

				fmt.Fprintln(
					rl.Stdout(),
					color.FgLightGreen.Render("SPOT")+" "+
						"DX de "+color.FgYellow.Render(spotCall)+
						m[2]+color.FgLightBlue.Render(freq)+
						m[4]+color.FgMagenta.Render(dxCall)+
						m[6]+commentColor.Render(comment)+
						m[8]+ts,
				)

				strings.ReplaceAll(spotCall, " ", "\x7f")
				strings.ReplaceAll(freq, " ", "\x7f")
				strings.ReplaceAll(dxCall, " ", "\x7f")
				strings.ReplaceAll(comment, " ", "\x7f")
				res := fc.SendAndWait(fmt.Sprintf("spot add rx_freq=%f callsign=%s spotter_callsign=%s comment=%s lifetime_seconds=%d", freqKhz/1000.0, dxCall, spotCall, comment, lifetimeSecs))
				if res.Error != 0 {
					log.Error().Uint32("error", res.Error).Msg(res.Message)
				}
			} else {
				var prompt = false
				for _, suffix := range promptSuffixes {
					if strings.HasSuffix(line, suffix) {
						rl.SetPrompt(color.FgMagenta.Render(strings.TrimSuffix(line, suffix)) + "> ")
						rl.Refresh()
						prompt = true
						break
					}
				}
				if !prompt {
					fmt.Fprintln(rl.Stdout(), line)
				}
			}
		}
		fc.Close()
		wg.Done()
	}()

	wg.Add(1)
	go func() {
		fc.Run()
		tc.Close()
		wg.Done()
	}()

	wg.Add(1)
	go func() {
		for {
			line, err := rl.Readline()
			if err != nil {
				break
			}
			if line == "" {
				continue
			}
			fmt.Fprintln(tc, line)
		}
		fc.Close()
		wg.Done()
	}()

	if cfg.Callsign != "" {
		time.Sleep(time.Second)
		fmt.Fprintf(tc, "%s\n", cfg.Callsign)
	}

	wg.Wait()
}
