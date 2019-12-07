package main

import (
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/boltdb/bolt"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/net/html/charset"
	"golang.org/x/text/encoding/charmap"
	"io"
	"io/ioutil"
	"log"
	"math/big"
	mrand "math/rand"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

type quote struct {
	Id, Date, Rating string
	Text             []string
}

type StatStruct struct {
	sync.RWMutex
	BucketCount, QuoteCount int
	Cryptocurrencies        map[string]map[string]float64
	CryptocurrenciesUpdated time.Time
}

type list struct {
	sync.RWMutex
	List []string
}

type WeatherStruct struct {
	Weather []struct {
		Description string `json:"description"`
	} `json:"weather"`
	Main struct {
		Temp     float64 `json:"temp"`
		Pressure float64 `json:"pressure"`
		Humidity int     `json:"humidity"`
		TempMin  float64 `json:"temp_min"`
		TempMax  float64 `json:"temp_max"`
	} `json:"main"`
	Wind struct {
		Speed float64 `json:"speed"`
		Deg   float64 `json:"deg"`
	} `json:"wind"`
	Sys struct {
		Country string `json:"country"`
	} `json:"sys"`
	CityID int    `json:"id"`
	Name   string `json:"name"`
	Cod    int    `json:"cod"`
}

//abort execution if error is encountered, used for unrecoverable errors
func CheckErr(e error) {
	if e != nil {
		log.Fatal(e)
	}
}

func Itob(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(v))
	return b
}

func getRandomInt(limit int64) int64 {
	nBig, err := rand.Int(rand.Reader, big.NewInt(limit))
	if err != nil { //in case of error fallback to math/rand
		log.Print(err)
		r := mrand.New(mrand.NewSource(time.Now().UnixNano()))
		return r.Int63n(limit)
	}
	return nBig.Int64()
}

//checks if given node is a <div> tag
func IsDiv(n *html.Node) bool {
	return n != nil && n.DataAtom == atom.Div
}

//checks if given node is a <span> tag
func IsSpan(n *html.Node) bool {
	return n != nil && n.DataAtom == atom.Span
}

//checks if given node is an <a> tag
func IsA(n *html.Node) bool {
	return n != nil && n.DataAtom == atom.A
}

func isTitle(n *html.Node) bool {
	return n.Type == html.ElementNode && n.Data == "title"
}

//gets attribute of an HTML tag
func GetAttribute(n *html.Node, key string) (string, error) {
	for _, attr := range n.Attr {
		if attr.Key == key {
			return attr.Val, nil
		}
	}
	return "", errors.New(key + "not exist in attribute")
}

/* GetQuotes recursively traverses HTML tree, extracts information from tags, and pushes it to global "cache" of quotes, Go buffered channel is used as cache.
All positions picked empirically specifically for bash.im website */
func GetQuotes(n *html.Node) {
	var (
		id, date, rating string
		text             []string
	)
	if IsDiv(n) {
		val, err := GetAttribute(n, "class")
		if err == nil && val == "actions" {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if IsA(c) {
					val, err := GetAttribute(c, "class")
					if err == nil && val == "id" {
						// extract quote's ID
						id = c.FirstChild.Data
					}
				}
				if IsSpan(c) {
					val, err := GetAttribute(c, "class")
					if err == nil && val == "rating-o" {
						// extract quote's rating
						rating = c.FirstChild.FirstChild.Data
					}
					if err == nil && val == "date" {
						//extract quote's date
						date = c.FirstChild.Data
					}
				}
			}
			// extract quote's text via sibling pointers in HTML tree
			TextDiv := n.NextSibling.NextSibling
			val, err := GetAttribute(TextDiv, "class")
			if err == nil && val == "text" {
				for c := TextDiv.FirstChild; c != nil; c = c.NextSibling {
					if c.Data != "br" {
						// and add it to slice of strings
						text = append(text, c.Data)
					}
				}
			}
		}
	}
	if id != "" {
		NewQuote := quote{
			Id:     id,
			Date:   date,
			Rating: rating,
			Text:   text,
		}

		QuoteIdInt, err := strconv.ParseUint(NewQuote.Id[1:], 10, 64) // skip first character '#'
		if err != nil {
			log.Print(err)
			return
		}

		BucketName := strconv.Itoa(int(QuoteIdInt - (QuoteIdInt % 1e3)))

		JsonBuf, err := json.Marshal(NewQuote)
		if err != nil {
			log.Print(err)
			return
		}

		buf := new(bytes.Buffer)
		compressor := zlib.NewWriter(buf)
		compressor.Write(JsonBuf)
		compressor.Close()

		err = db.Update(func(tx *bolt.Tx) error {
			bucket, e := tx.CreateBucketIfNotExists([]byte(BucketName))
			if e != nil {
				return e
			}
			return bucket.Put(Itob(QuoteIdInt), buf.Bytes())
		})
		if err != nil {
			log.Print(err)
			return
		}
	}
	//recursively parse HTML tag through children of current node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		GetQuotes(c)
	}
}

//helper to get quotes
func FetchQuote(RequestedId int64) (QuoteBuf *quote) {
	buffer := new(bytes.Buffer)
	err := db.View(func(tx *bolt.Tx) error {
		for {
			id := int64(0)
			if RequestedId == 0 {
				configLock.RLock()
				id = getRandomInt(configuration.RandomLimit)
				configLock.RUnlock()
			} else {
				id = RequestedId
			}
			BucketName := strconv.Itoa(int(id - (id % 1e3)))
			bucket := tx.Bucket([]byte(BucketName))
			if bucket == nil {
				if RequestedId != 0 {
					return fmt.Errorf("quote and bucket not found")
				}
				continue
			}
			buf := bucket.Get(Itob(uint64(id)))
			if buf == nil {
				if RequestedId != 0 {
					return fmt.Errorf("quote not found")
				}
				continue
			}
			buffer.Write(buf)
			src, err := zlib.NewReader(buffer)
			if err != nil {
				return err
			}
			JsonBuf := new(bytes.Buffer)
			io.Copy(JsonBuf, src)
			src.Close()
			err = json.Unmarshal(JsonBuf.Bytes(), &QuoteBuf)
			if err != nil {
				return err
			}
			return nil
		}
	})
	if err != nil {
		log.Print(err)
		return
	}
	log.Printf("Quote id: %s\n", QuoteBuf.Id)
	return
}

func crawl() {
	configLock.RLock()
	ticker := time.NewTicker(configuration.CrawlTimeout * time.Second)
	configLock.RUnlock()
	for range ticker.C {
		configLock.RLock()
		page, err := getHTMLpage(configuration.BashUrl)
		configLock.RUnlock()

		if err != nil {
			log.Printf("Error during crawl: %s", err)
			continue
		}

		GetQuotes(page)
		GatherStats()
	}
}

func backup() {
	configLock.RLock()
	ticker := time.NewTicker(configuration.BackupTimeout * time.Second)
	configLock.RUnlock()
	for range ticker.C {
		file, err := os.Create(".boltdb.backup")
		if err != nil {
			log.Print(err)
			continue
		}

		err = db.View(func(tx *bolt.Tx) error {
			_, err := tx.WriteTo(file)
			return err
		})
		file.Close()
		if err != nil {
			log.Print(err)
			continue
		}
		os.Rename(".boltdb.backup", "boltdb.backup")
	}
}

func GatherStats() {
	QuoteCount, BucketCount := 0, 0
	err := db.View(func(tx *bolt.Tx) error {
		cursor := tx.Cursor()
		LastKey, _ := cursor.Last()
		for k, _ := cursor.First(); !bytes.Equal(k, LastKey); k, _ = cursor.Next() {
			QuoteCount += tx.Bucket(k).Stats().KeyN
		}
		BucketCount = cursor.Bucket().Stats().BucketN
		return nil
	})
	if err != nil {
		log.Print(err)
		return
	}
	stats.Lock()
	stats.QuoteCount = QuoteCount
	stats.BucketCount = BucketCount
	stats.Unlock()
	CryptocurrenciesUpdate()
}

func CryptocurrenciesUpdate() error {
	var raw interface{}

	configLock.RLock()
	response, err := getRawfile(configuration.APIJsonLink)
	configLock.RUnlock()
	if err != nil {
		return err
	}
	body, err := ioutil.ReadAll(response.Body)
	err = json.Unmarshal(body, &raw)
	response.Body.Close()
	if err != nil {
		return err
	}
	NewCryptocurrencies := make(map[string]map[string]float64)
	array := raw.(map[string]interface{})
	for k, v := range array {
		switch v := v.(type) {
		case map[string]interface{}:
			for key, value := range v {
				switch value := value.(type) {
				case float64:
					_, ok := NewCryptocurrencies[k]
					if !ok {
						NewCryptocurrencies[k] = make(map[string]float64)
					}
					NewCryptocurrencies[k][key] = value
				default:
					return fmt.Errorf("unexpected type %s in JSON", value)
				}
			}
		default:
			return fmt.Errorf("unexpected type %s in outer JSON", v)
		}
	}
	stats.Lock()
	stats.Cryptocurrencies = NewCryptocurrencies
	stats.CryptocurrenciesUpdated = time.Now()
	stats.Unlock()
	return nil
}

func Str2cp1251(input string) (result string) {
	convert := charmap.Windows1251.NewEncoder()
	result, err := convert.String(input)
	if err != nil {
		var (
			bytes    []byte
			TempRune byte
		)

		log.Printf("Error: %s\n", err)

		for _, v := range input {
			TempRune, _ = charmap.Windows1251.EncodeRune(v)
			bytes = append(bytes, TempRune)
		}
		result = string(bytes)
	}
	return
}

func getHTMLpage(url string) (page *html.Node, err error) {
	response, err := getRawfile(url)
	if err != nil {
		return nil, err
	}

	utf8, err := charset.NewReader(response.Body, response.Header.Get("Content-Type"))
	if err != nil {
		return nil, err
	}

	page, err = html.Parse(utf8)
	if err != nil {
		return nil, err
	}

	response.Body.Close()
	return

}

func getRawfile(url string) (response *http.Response, err error) {
	client := &http.Client{
		Timeout: time.Second * 10,
	}
	request, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	configLock.RLock()
	userAgent := configuration.UserAgent
	configLock.RUnlock()
	request.Header.Set("User-Agent", userAgent)
	response, err = client.Do(request)
	if err != nil {
		return nil, err
	}

	return
}

func extractTitle(n *html.Node) (string, bool) {
	if isTitle(n) && n.FirstChild != nil {
		return n.FirstChild.Data, true
	}

	for i := n.FirstChild; i != nil; i = i.NextSibling {
		result, ok := extractTitle(i)
		if ok {
			return result, ok
		}
	}

	return "", false
}

func pruneTitleList() {
	configLock.RLock()
	ticker := time.NewTicker(configuration.TitleTimeout * time.Second)
	configLock.RUnlock()

	for range ticker.C {
		titleList.Lock()
		if len(titleList.List) == 0 {
			titleList.Unlock()
			continue
		}
		titleList.List = titleList.List[1:]
		titleList.Unlock()
	}
}

func getWeather(city string) (weather *WeatherStruct, err error) {
	configLock.RLock()
	response, err := getRawfile(configuration.WeatherURL + "&q=" + city + "&APPID=" + configuration.WeatherToken)
	configLock.RUnlock()
	defer response.Body.Close()
	if err != nil {
		return nil, err
	}
	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	weather = new(WeatherStruct)
	err = json.Unmarshal(body, weather)
	if err != nil {
		return nil, err
	}
	return
}
