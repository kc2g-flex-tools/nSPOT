package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
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

const SpotNotFoundError = 0x500000BC

var cfg struct {
	RadioIP       string
	Station       string
	Callsign      string
	ClusterServer string
	QRT           bool
	OnePerBand    bool
	Timeout       time.Duration
}

func init() {
	flag.StringVar(&cfg.RadioIP, "radio", ":discover:", "radio IP address or discovery spec")
	flag.StringVar(&cfg.Station, "station", "Flex", "station name to bind to")
	flag.StringVar(&cfg.Callsign, "callsign", "", "callsign for login")
	flag.StringVar(&cfg.ClusterServer, "server", "", "cluster server to connect to")
	flag.DurationVar(&cfg.Timeout, "timeout", 5*time.Minute, "spot persistence timeout")
	flag.BoolVar(&cfg.QRT, "qrt", true, "delete spots with QRT in comment")
	flag.BoolVar(&cfg.OnePerBand, "one-per-band", true, "expect a given callsign only once per band")
}

func logToConsole(w io.Writer, m []string, remove bool) {
	commentColor := color.FgLightCyan
	if remove {
		commentColor = color.FgLightRed
	}

	freqKhz, err := strconv.ParseFloat(m[3], 64)
	if err != nil {
		log.Error().Err(err).Send()
		return
	}
	fmt.Fprintln(
		w,
		color.FgLightGreen.Render("DX de")+
			" "+color.FgYellow.Render(m[1])+
			m[2]+color.FgLightBlue.Render(m[3])+
			m[4]+color.FgMagenta.Render(m[5])+
			m[6]+commentColor.Render(m[7])+
			m[8]+m[9]+
			" "+color.FgLightGreen.Render(getBand(freqKhz)),
	)
}

type bandEdge struct {
	minFreq float64
	name    string
}

var bandEdges = []bandEdge{
	{0, "LFMF"},
	{1800, "160m"},
	{3500, "80m"},
	{5000, "60m"},
	{7000, "40m"},
	{10000, "30m"},
	{14000, "20m"},
	{18000, "17m"},
	{21000, "15m"},
	{24890, "12m"},
	{26000, "11m"},
	{28000, "10m"},
	{50000, "6m"},
	{144000, "2m"},
	{220000, "125cm"},
	{420000, "70cm"},
	{900000, "900M"},
	{1240000, "1240M"},
	{2300000, "microwave"},
}

func getBand(freq float64) string {
	i := 0
	for i < len(bandEdges)-1 && freq >= bandEdges[i+1].minFreq {
		i++
	}
	return bandEdges[i].name
}

type spotKey struct {
	freq string
	call string
}

type spot struct {
	id      int
	expires time.Time
}

var spotIds = map[spotKey]spot{}

func sendToFlex(fc *flexclient.FlexClient, m []string, remove bool) {
	spotCall, freq, dxCall, comment := m[1], m[3], m[5], m[7]
	freqKhz, err := strconv.ParseFloat(freq, 64)
	if err != nil {
		log.Error().Err(err).Send()
		return
	}
	var key spotKey
	if cfg.OnePerBand {
		key = spotKey{freq: getBand(freqKhz), call: dxCall}
	} else {
		key = spotKey{freq: fmt.Sprintf("%.0f", freqKhz), call: dxCall} // round to nearest kHz
	}

	strings.ReplaceAll(spotCall, " ", "\x7f")
	strings.ReplaceAll(freq, " ", "\x7f")
	strings.ReplaceAll(dxCall, " ", "\x7f")
	strings.ReplaceAll(comment, " ", "\x7f")

	if remove {
		removeSpot(fc, key)
	} else {
		addSpot(fc, key, spotCall, freqKhz, dxCall, comment)
	}
}

func addSpot(fc *flexclient.FlexClient, key spotKey, spotCall string, freqKhz float64, dxCall, comment string) {
	lifetimeSecs := int(cfg.Timeout / time.Second)
	fields := fmt.Sprintf("rx_freq=%f callsign=%s spotter_callsign=%s comment=%s lifetime_seconds=%d", freqKhz/1000.0, dxCall, spotCall, comment, lifetimeSecs)

	var res flexclient.CmdResult
	sp, existed := spotIds[key]
	if existed {
		// Spot already exists for band/mode, update instead of adding
		res = fc.SendAndWait(fmt.Sprintf("spot set %d %s", sp.id, fields))
	}
	if !existed || res.Error == SpotNotFoundError {
		res = fc.SendAndWait(fmt.Sprintf("spot add %s", fields))
	}

	if res.Error != 0 {
		log.Error().Uint32("error", res.Error).Msg(res.Message)
		return
	}

	if !existed {
		id, err := strconv.Atoi(res.Message)
		if err != nil {
			log.Error().Err(err).Msg("atoi")
			return
		}
		sp.id = id
	}
	spotIds[key] = spot{id: sp.id, expires: time.Now().Add(cfg.Timeout)}
}

func removeSpot(fc *flexclient.FlexClient, key spotKey) {
	spot, ok := spotIds[key]
	if ok {
		res := fc.SendAndWait(fmt.Sprintf("spot remove %d", spot.id))
		if res.Error != 0 && res.Error != SpotNotFoundError {
			log.Error().Uint32("error", res.Error).Msg(res.Message)
		}
	}
	delete(spotIds, key)
}

func cleanupSpots() {
	now := time.Now()
	for k, v := range spotIds {
		if v.expires.Before(now) {
			delete(spotIds, k)
		}
	}
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
	}()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		fc.Run()
		tc.Close()
		rl.Close()
		wg.Done()
	}()

	go func() {
		lines := bufio.NewScanner(tc)
		for lines.Scan() {
			line := lines.Text()
			if m := spotPattern.FindStringSubmatch(line); m != nil {
				remove := cfg.QRT && qrtPattern.MatchString(m[7])
				logToConsole(rl.Stdout(), m, remove)
				sendToFlex(fc, m, remove)
				cleanupSpots()
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
	}()

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
	}()

	if cfg.Callsign != "" {
		time.Sleep(time.Second)
		fmt.Fprintf(tc, "%s\n", cfg.Callsign)
	}

	wg.Wait()
}
