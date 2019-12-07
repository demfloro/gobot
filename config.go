package main

import (
	"time"
)

type config struct {
	Nick, Realname, Channel, Password, APIJsonLink                            string
	Server, BashUrl, Database, WeatherURL, WeatherToken, UserAgent            string
	TLS, InsecureTLS                                                          bool
	AdminHosts, Ignore                                                        []string
	CommandTimeout, BackupTimeout, CrawlTimeout, TitleTimeout, WeatherTimeout time.Duration
	MaxLineLength, TitleLimit                                                 int
	RandomLimit                                                               int64
	CityMap                                                                   map[string]string
}

// Random_limit should be about total quantity of quotes
// IRC PRIVMSG: : + NICK + ! + USER + @ + HOST + <space> + PRIVMSG + <space> + Channel + <space> + : + \r\n
//              1 + var  + 1 + var  + 1 + var  +    1    +    6    +    1    +    var  +    1    + 1 +  2
// total: 15 + vars bytes
