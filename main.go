package main

import (
	"crypto/tls"
	"fmt"
	"github.com/BurntSushi/toml"
	"github.com/boltdb/bolt"
	"github.com/demfloro/go-ircevent"
	"golang.org/x/text/encoding/charmap"
	"log"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	mmHgMagic = 0.750062
)

var (
	timer         = time.NewTimer(time.Millisecond) //timer has to fire right away to handle first request
	weatherTimer  = time.NewTimer(time.Millisecond)
	configuration config
	configLock    sync.RWMutex
	titleList     list
	db            *bolt.DB
	stats         StatStruct
	// expression to match anything between '!NNN' or !NNNNN and !NNN-NNN !NNNNN-NNN
	re      = regexp.MustCompile("^\\![\\w]{3,5}(\\-[\\w]{3})?$")
	splitRe = regexp.MustCompile("-") //for splitting user command
	isUrl   = regexp.MustCompile("https?://[^\\s]+")
	convert = charmap.Windows1251.NewDecoder()
)

func init() {
	// no need to lock because no other writer is yet possible
	_, err := toml.DecodeFile("config.toml", &configuration)
	CheckErr(err)
	stats.Cryptocurrencies = make(map[string]map[string]float64)
}

func PrivmsgHandler(e *irc.Event) {
	go func(e *irc.Event) {
		message := e.Message()
		configLock.RLock()
		switch message {
		case "!gtfo":
			for _, i := range configuration.AdminHosts {
				if e.Host == i {
					e.Connection.Quit()
					break
				}
			}
		case "!stats":
			for _, i := range configuration.AdminHosts {
				if e.Host == i {
					go StatsHandler(e)
					break
				}
			}
		case "!reload":
			for _, i := range configuration.AdminHosts {
				if e.Host == i {
					go ReloadHandler(e)
					break
				}
			}
		case "!id":
			for _, i := range configuration.AdminHosts {
				if e.Host == i {
					go IdentifyHandler(e)
					break
				}
			}
		default:
			if strings.HasPrefix(message, "!bash") {
				select {
				case <-timer.C:
					timer.Reset(configuration.CommandTimeout * time.Second)
					go BashHandler(e)
				default:
				}
			}
			tmpMessage, _ := convert.String(message)
			if strings.HasPrefix(message, "!w") || strings.HasPrefix(tmpMessage, "!п") {
				select {
				case <-weatherTimer.C:
					weatherTimer.Reset(configuration.WeatherTimeout * time.Second)
					go WeatherHandler(e)
				default:
				}
			}
			if strings.HasPrefix(message, "!") {
				go CryptocurrencyHandler(e)
			}
			if isUrl.MatchString(message) {
				for _, nick := range configuration.Ignore {
					if e.Nick == nick {
						return
					}
					go TitleHandler(e)
				}
			}
		}
		configLock.RUnlock()
	}(e)
}

func StatsHandler(e *irc.Event) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	stats.RLock()
	e.Connection.Privmsgf(e.Arguments[0], "goroutines: %d, OS threads: %d, heap: %d KB, GC runs: %d, quote count: %d, prices updated %s, runtime: %s",
		runtime.NumGoroutine(),
		runtime.GOMAXPROCS(0),
		m.HeapAlloc/1024,
		m.NumGC,
		stats.QuoteCount,
		stats.CryptocurrenciesUpdated.Format(time.UnixDate),
		runtime.Version())
	stats.RUnlock()
}

func BashHandler(e *irc.Event) {
	var QuoteBuf *quote
	if message := e.Message(); 6 <= len(message) {
		id, err := strconv.ParseInt(message[6:], 10, 64)
		if err != nil {
			e.Connection.Privmsgf(e.Arguments[0], "can't parse quote id")
			return
		}
		QuoteBuf = FetchQuote(id)
	} else {
		QuoteBuf = FetchQuote(0)
	}
	if QuoteBuf == nil {
		e.Connection.Privmsgf(e.Arguments[0], "quote not found")
		return
	}

	/* coloration:
	\x03 - "braces" for coloring
	04 - red
	10 - dark cyan
	03 - green
	*/

	e.Connection.Privmsgf(e.Arguments[0], "%s :: %s :: Rating: %s", "\x0304"+QuoteBuf.Id+"\x03", "\x0310"+QuoteBuf.Date+"\x03", "\x0304"+QuoteBuf.Rating+"\x03")
	for _, i := range QuoteBuf.Text {
		if strings.Trim(i, " ") == "" { //trim all empty lines
			continue
		}
		cp1251_i := Str2cp1251(i)

		configLock.RLock()
		if len(cp1251_i) > configuration.MaxLineLength {
			LastWhitespace, LastWrap, i := 0, 0, 0
			j := cp1251_i[0]
			for {
				j = cp1251_i[i]
				if string(cp1251_i[j]) == " " {
					LastWhitespace = i
				}
				if i == (len(cp1251_i) - 1) {
					e.Connection.Privmsgf(e.Arguments[0], "%s", "\x0303"+
						cp1251_i[LastWrap:]+"\x03")
					break
				}

				if (i - LastWrap + 1) == configuration.MaxLineLength {
					e.Connection.Privmsgf(e.Arguments[0], "%s", "\x0303"+
						cp1251_i[LastWrap:LastWhitespace+1]+"\x03")
					LastWrap, i = LastWhitespace+1, LastWhitespace+1
					time.Sleep(time.Second)
				}
				i++
			}
		} else {
			e.Connection.Privmsgf(e.Arguments[0], "%s", "\x0303"+cp1251_i+"\x03")
		}
		configLock.RUnlock()
	}
}

func JoinHandler(e *irc.Event) {
	go func(e *irc.Event) {
		IdentifyHandler(e)
		configLock.RLock()
		e.Connection.Join(configuration.Channel)
		configLock.RUnlock()
	}(e)
}

func IdentifyHandler(e *irc.Event) {
	configLock.RLock()
	if e.Connection.GetNick() != configuration.Nick {
		e.Connection.Privmsgf("NickServ", "ghost %s %s",
			configuration.Nick, configuration.Password)
		e.Connection.Nick(configuration.Nick)
	}
	e.Connection.Privmsgf("NickServ", "identify %s", configuration.Password)
	configLock.RUnlock()
}

func CryptocurrencyHandler(e *irc.Event) {
	var (
		message  = e.Message()
		format   = "\x0304%s/%s: %.2f\x03"
		ageFloat float64
	)

	matched := re.MatchString(message)
	if !matched {
		return
	}

	message = strings.TrimPrefix(message, "!")
	output := splitRe.Split(message, 2)

	fiat := "USD"
	if len(output) == 2 {
		fiat = strings.ToUpper(output[1])
	}

	currency := strings.ToUpper(output[0])

	stats.RLock()
	price, ok := stats.Cryptocurrencies[currency][fiat]
	updated := stats.CryptocurrenciesUpdated
	stats.RUnlock()

	if !ok {
		return
	}

	if currency == "BCN" {
		format = "\x0304%s/%s: %.4f\x03"
	}

	age := time.Since(updated)
	if age.Seconds() >= 60 {
		ageFloat = age.Minutes()
		format += " (age: %.0f minutes)"
	} else {
		ageFloat = age.Seconds()
		format += " (age: %.0f seconds)"
	}

	e.Connection.Privmsgf(e.Arguments[0], format, currency, fiat, price, ageFloat)
}

func ReloadConfig() (err error) {
	var NewConfiguration config

	_, err = toml.DecodeFile("config.toml", &NewConfiguration)
	if err != nil {
		return
	}

	configLock.Lock()
	log.Print("config lock acquired")
	configuration = NewConfiguration
	configLock.Unlock()
	log.Print("config lock released")

	return CryptocurrenciesUpdate()
}

func ReloadHandler(e *irc.Event) {
	if err := ReloadConfig(); err != nil {
		e.Connection.Privmsgf(e.Arguments[0], "failed: %s", err)
		return
	}
	e.Connection.Privmsgf(e.Arguments[0], "reload complete")
}

func HUPHandler(signals <-chan os.Signal) {
	for range signals {
		log.Print("Got HUP signal")
		if err := ReloadConfig(); err != nil {
			log.Printf("failed to reload config: %s", err)
		} else {
			log.Print("reload complete")
		}
	}
}

func TitleHandler(e *irc.Event) {
	if strings.Contains(e.Message(), "bash.im") {
		return
	}

	configLock.RLock()
	titleLimit := configuration.TitleLimit
	configLock.RUnlock()

	urls := isUrl.FindAllString(e.Message(), titleLimit)
	if urls == nil {
		return
	}

	titleList.RLock()
	if len(titleList.List) >= titleLimit {
		titleList.RUnlock()
		return
	}
	titleList.RUnlock()

OUTER:
	for _, url := range urls {
		page, err := getHTMLpage(url)
		if err != nil {
			log.Print(err)
			continue
		}

		title, ok := extractTitle(page)
		if !ok {
			continue
		}
		titleList.Lock()
		for _, v := range titleList.List {
			if v == title {
				titleList.Unlock()
				continue OUTER
			}
		}
		if len(titleList.List) < titleLimit {
			titleList.List = append(titleList.List, title)
		}
		titleList.Unlock()
		e.Connection.Privmsgf(e.Arguments[0], "^:: %s", Str2cp1251(title))
	}
}

func WeatherHandler(e *irc.Event) {
	var (
		direction   string
		origMessage = e.Message()
		weather     *WeatherStruct
	)
	message, err := convert.String(origMessage)
	if err != nil {
		log.Print(err)
	}

	message = strings.ToLower(strings.TrimPrefix(message, "!w "))
	message = strings.ToLower(strings.TrimPrefix(message, "!п "))
	origMessage = strings.ToLower(strings.TrimPrefix(origMessage, "!w "))
	origMessage = strings.ToLower(strings.TrimPrefix(origMessage, "!п "))
	if message == "" {
		weatherTimer.Reset(time.Millisecond)
		return
	}
	log.Printf("Looking for %s...", message)

	configLock.RLock()
	city, ok := configuration.CityMap[message]
	origCity, origOk := configuration.CityMap[origMessage]
	configLock.RUnlock()

	if !ok && !origOk {
		log.Printf(" Not found\n")
		weatherTimer.Reset(time.Millisecond)
		return
	}

	log.Printf(" Found\n")
	if !ok {
		weather, err = getWeather(origCity)
	} else {
		weather, err = getWeather(city)
	}
	if err != nil {
		log.Print(err)
		e.Connection.Privmsgf(e.Arguments[0], "fail: %s", err)
		return
	}
	if weather.Cod == 404 {
		log.Printf("%s not found\n", city)
		e.Connection.Privmsgf(e.Arguments[0], "%s not found", city)
		return
	}
	deg := weather.Wind.Deg
	switch {
	case (0 <= deg && deg < 22.5) || (337.5 <= deg && deg <= 360):
		direction = "северный"
	case 22.5 <= deg && deg < 67.5:
		direction = "северо-восточный"
	case 67.5 <= deg && deg < 112.5:
		direction = "восточный"
	case 112.5 <= deg && deg < 157.5:
		direction = "юго-восточный"
	case 157.5 <= deg && deg < 202.5:
		direction = "южный"
	case 202.5 <= deg && deg < 247.5:
		direction = "юго-западный"
	case 247.5 <= deg && deg < 292.5:
		direction = "западный"
	case 292.5 <= deg && deg < 337.5:
		direction = "северо-западный"
	}
	output := fmt.Sprintf("%s/%s: %s; температура: %.1f °C; давление: %.1f мм рт.ст; ветер: %s (%.0f°), %.1f м/с, относ. влаж.: %d%%",
		weather.Name, weather.Sys.Country, weather.Weather[0].Description,
		weather.Main.Temp, weather.Main.Pressure*mmHgMagic,
		direction, deg, weather.Wind.Speed, weather.Main.Humidity)
	e.Connection.Privmsg(e.Arguments[0], Str2cp1251(output))

}

func main() {
	var err error

	configLock.RLock()
	db, err = bolt.Open(configuration.Database, 0600, &bolt.Options{Timeout: time.Second})
	CheckErr(err)
	defer db.Close()

	go crawl()
	go backup()
	go GatherStats()
	go pruneTitleList()

	irccon := irc.IRC(configuration.Nick, configuration.Realname)

	if configuration.TLS {
		irccon.UseTLS = true
		if configuration.InsecureTLS {
			irccon.TLSConfig = &tls.Config{InsecureSkipVerify: true}
		}
	}
	irccon.AddCallback("001", JoinHandler)
	irccon.AddCallback("PRIVMSG", PrivmsgHandler)

	err = irccon.Connect(configuration.Server)
	configLock.RUnlock()
	CheckErr(err)

	HUPChan := make(chan os.Signal)
	defer close(HUPChan)
	go HUPHandler(HUPChan)
	signal.Notify(HUPChan, syscall.SIGHUP)

	defer irccon.Quit()

	irccon.Loop()
}
