package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"gopkg.in/gomail.v1"
)

const (
	RETRY_REQUEST_TIME         = 5                //每个文档，重试次数
	CRAWL_TOTAL_LONG_INTERVAL  = time.Minute * 30 //每次整体爬虫的间隔时间
	CRAWL_TOTAL_SHORT_INTERVAL = time.Minute * 10 //每次整体爬虫的间隔时间
	CRAWL_DOC_INTERVAL         = time.Second * 1  //每次爬虫请求的间隔时间

	MAX_NO_NEW_SHORT_CRAWL = 3 //达到此阀值则间隔时间设为长间隔
)

var noNewCounter int8 = 0
var crawlInterval time.Duration = CRAWL_TOTAL_SHORT_INTERVAL

var config Config
var crawlUrl, crawlUser, jsonPath string
var lastPubTime time.Time //爬过的最后回复时间
var email = &Email{}
var crawlMutex sync.Mutex

type Reply struct {
	Id      string
	Quote   string
	Content string
	Time    string
}

type Topic struct {
	Id     string
	Title  string
	Url    string
	User   string
	Replys map[string]*Reply
}

type Email struct {
	Addr     string
	User     string
	Password string
	Port     int
	To       string
}

func init() {
	config = ReadConf("./config.json")
	crawlUrl = config.Get("Url").(string)
	crawlUser = config.Get("UserName").(string)

	email.Addr = config.Get("EmailAddr").(string)
	email.Port = int(config.Get("EmailPort").(float64))
	email.User = config.Get("EmailUser").(string)
	email.Password = config.Get("EmailPassword").(string)
	email.To = config.Get("EmailTo").(string)

	if crawlUrl == "" || crawlUser == "" {
		panic("Url or Username is empty")
	}

	jsonPath = filepath.Join("./", crawlUser)
	err := ensureDir(jsonPath)
	if err != nil {
		panic(err)
	}
	readLastCrawlTime()

	log.SetFlags(log.Ldate | log.Ltime)
}

func readTodayTopics() map[string]*Topic {
	topicList := make(map[string]*Topic, 0)
	today := today()
	b, err := ioutil.ReadFile(filepath.Join(jsonPath, today+".json"))
	if len(b) == 0 {
		b = []byte("{}")
	}
	err = json.Unmarshal(b, &topicList)
	if err != nil {
		log.Println(err)
	}

	return topicList
}

func main() {
	startCrawl()
	startServer()
}

func startServer() {
	port := config.Get("Port").(string)
	if port == "" {
		port = "8090"
	}

	http.Handle("/", http.HandlerFunc(handlerFunc))
	log.Println("listening on:" + port)
	http.ListenAndServe(":"+port, nil)
}

func handlerFunc(resp http.ResponseWriter, req *http.Request) {
	path := req.URL.Path
	dateReg := regexp.MustCompile(`/topics/20(\d){2}-(\d){2}-(\d){2}`)
	switch {
	case dateReg.MatchString(path):
		date := strings.Split(path, "/")[2]
		respTopics(date, resp)
	case path == "/topics/today":
		respTopics(today(), resp)
	case path == "/topics/yesterday":
		respTopics(yesterday(), resp)
	case path == "/api/crawl":
		go tickCrawl()
		resp.Write([]byte("ok"))
	default:
		resp.Write([]byte("no new"))
	}
}

func respTopics(date string, resp http.ResponseWriter) {
	respTopicList := make(map[string]*Topic, 0)

	b, err := ioutil.ReadFile(filepath.Join(jsonPath, date+".json"))
	if len(b) == 0 {
		b = []byte("{}")
	}

	err = json.Unmarshal(b, &respTopicList)
	if err != nil {
		log.Println(err)
		resp.Write([]byte("inner error"))
		return
	}

	if len(respTopicList) > 0 {
		html := renderHtml(respTopicList)
		resp.Header().Set("Content", "txt/html")
		resp.Write([]byte(html))
	} else {
		resp.Write([]byte("no new"))
	}
}

func renderHtml(topicList map[string]*Topic) string {
	topic_keys := make([]string, 0)
	for key, _ := range topicList {
		topic_keys = append(topic_keys, key)
	}

	sort.StringSlice(topic_keys).Sort()

	htmlHead := `<!DOCTYPE html>
		<html>
			<head>
			<title>topic and reply</title>
			</head>
		`
	htmlList := ""
	for _, key := range topic_keys {
		v, _ := topicList[key]
		listHead := fmt.Sprintf(
			`<li>
				<a href=%s style='text-decoration: none;' target=\"blank\">%s -- %s</a>
				</br>
				<ul>`,
			v.Url, v.Title, v.User)
		listBody := ""

		reply_keys := make([]string, 0)
		for key, _ := range v.Replys {
			reply_keys = append(reply_keys, key)
		}

		sort.StringSlice(reply_keys).Sort()

		for _, key := range reply_keys {
			reply, _ := v.Replys[key]
			listBody += fmt.Sprintf(
				`<li>
					<quote>
						%s
					</quote>
					<h3>
						%s
					</h3>
					<h5>
						%s
					</h5>
				</li>`, reply.Quote, reply.Content, reply.Time)
		}
		listFooter := `
			</ul>
			</li>`

		htmlList += listHead + listBody + listFooter
	}
	htmlBody := "<body><ol>" + htmlList + "</ol></body>"
	htmlFooter := `
		</html>
		`
	return htmlHead + htmlBody + htmlFooter
}

func sendMail(content string) {
	if email.Addr == "" {
		return
	}

	msg := gomail.NewMessage()
	msg.SetHeader("From", email.User)
	msg.SetHeader("To", email.To)
	msg.SetHeader("Subject", "new topics")
	msg.SetBody("text/html", content)

	mailer := gomail.NewMailer(email.Addr, email.User, email.Password, email.Port)
	if err := mailer.Send(msg); err != nil {
		log.Println(err)
	}
}
func startCrawl() {
	go func() {
		for {
			runtime.Gosched()
			tickCrawl()

			fmt.Println(crawlInterval)
			time.Sleep(crawlInterval)
		}
	}()
}

func tickCrawl() {
	defer func() {
		if r := recover(); r != nil {
			log.Println(r)
		}
	}()

	crawlMutex.Lock()
	defer crawlMutex.Unlock()

	hasNew, topicList := crawl()
	if !hasNew {
		log.Println("no new")
		noNewCounter++
		if noNewCounter >= MAX_NO_NEW_SHORT_CRAWL {
			crawlInterval = CRAWL_TOTAL_LONG_INTERVAL
		}
		return
	}

	log.Println("find new")
	noNewCounter = 0
	crawlInterval = CRAWL_TOTAL_SHORT_INTERVAL

	save(topicList)
	sendMail(renderHtml(topicList))
}
func crawl() (bool, map[string]*Topic) {
	topicList := readTodayTopics()
	hasNew := false
	log.Println("crawl time: ", time.Now())

	doc, err := goquery.NewDocument(crawlUrl)
	if err != nil {
		log.Println(err)
		return false, topicList
	}

	topics_root := doc.Find(".olt").First()

	if topics_root.Length() == 0 {
		return false, topicList
	}

	topic_nodes := topics_root.Find("tr")
	topic_nodes = topic_nodes.Slice(1, len(topic_nodes.Nodes))
	page_num := topic_nodes.Length()

	this_crawl_last_time := lastPubTime
	log.Println("last crawl time: ", this_crawl_last_time)

	topic_nodes.Each(func(i int, s_topic *goquery.Selection) {
		s_title := s_topic.Find("td").First().Find("a").First()
		topic_url, b := s_title.Attr("href")
		topic_title, b := s_title.Attr("title")
		topic_user := s_topic.Find("td").First().Next().Find("a").Text()
		if !b {
			return
		}
		arr := strings.Split(topic_url, "/")
		topic_id := arr[len(arr)-2]

		docNew := requestDoc(topic_url, RETRY_REQUEST_TIME)
		if docNew == nil {
			log.Println("error after retry")
			return
		}

		paginator := docNew.Find(".paginator")
		reply_page_num := 1
		if paginator != nil {
			num, b := paginator.Find(".thispage").First().Attr("data-total-page")
			if b {
				reply_page_num, _ = strconv.Atoi(num)
			}
		}

		for k := 0; k < reply_page_num; k++ {
			reply_url := topic_url + "?start=" + strconv.Itoa(k*100)
			docNew := requestDoc(reply_url, RETRY_REQUEST_TIME)

			replys := docNew.Find("#comments").First().Find("li")
			replys.Each(func(j int, s_reply *goquery.Selection) {
				reply_id, b := s_reply.Attr("id")
				if !b {
					return
				}

				doc := s_reply.Find(".reply-doc").First()
				reply_time := doc.Find(".pubtime").First().Text()

				name := doc.Find(".bg-img-green").Find("a").First().Text()
				if name != crawlUser {
					return
				}

				this_time, err := time.ParseInLocation("2006-01-02 15:04:05", reply_time, time.Local)
				if err != nil {
					log.Println(err)
					return
				}

				if this_time.After(lastPubTime) {
					if this_time.After(this_crawl_last_time) {
						this_crawl_last_time = this_time
					}
				} else {
					return
				}

				quote := ""
				quote_node := doc.Find(".reply-quote")
				if quote_node != nil {
					quote = quote_node.Find(".all").First().Text()
				}
				content := doc.Find("p").First().Text()

				reply := Reply{
					Id:      reply_id,
					Quote:   quote,
					Content: content,
					Time:    reply_time,
				}

				if _, b := topicList[topic_id]; !b {
					topic := new(Topic)
					topic.Id = topic_id
					topic.Replys = make(map[string]*Reply, 0)
					topic.Url = topic_url
					topic.Title = topic_title
					topic.User = topic_user
					topicList[topic_id] = topic
				}

				if _, b := topicList[topic_id].Replys[reply_id]; !b {
					topicList[topic_id].Replys[reply_id] = &reply
					log.Println("find one")
					hasNew = true
				}
			})
			log.Println(fmt.Sprintf("crawl reply %d/%d of page %d/%d", k+1, reply_page_num, i+1, page_num))
			time.Sleep(CRAWL_DOC_INTERVAL)
		}
	})

	lastPubTime = this_crawl_last_time
	return hasNew, topicList
}

func requestDoc(url string, num int) *goquery.Document {
	docNew, err := goquery.NewDocument(url)
	if err != nil {
		log.Println(err)
		if num > 0 {
			time.Sleep(CRAWL_DOC_INTERVAL)
			log.Println("retry request")
			return requestDoc(url, num-1)
		} else {
			return nil
		}
	}
	return docNew
}

func today() string {
	year, month, day := time.Now().Date()
	return fmt.Sprintf("%d-%02d-%02d", year, int(month), day)
}

func yesterday() string {
	year, month, day := time.Now().Add(-24 * time.Hour).Date()
	return fmt.Sprintf("%d-%02d-%02d", year, int(month), day)
}

func save(topicList map[string]*Topic) (err error) {
	err = saveTopicList(topicList)
	if err != nil {
		return
	}

	return writeLastCrawlTime()
}

func saveTopicList(topicList map[string]*Topic) error {
	b, err := json.MarshalIndent(topicList, "", "	")
	if err != nil {
		return err
	}

	s := string(b)
	if strings.EqualFold(s, "{}") {
		return nil
	}

	err = ioutil.WriteFile(filepath.Join(jsonPath, today()+".json"), b, 0777)
	return err
}

func writeLastCrawlTime() error {
	return ioutil.WriteFile(filepath.Join(jsonPath, "lastPubDate.txt"), []byte(lastPubTime.String()), 0777)
}

func readLastCrawlTime() {
	b, _ := ioutil.ReadFile(filepath.Join(jsonPath, "lastPubDate.txt"))
	if len(b) == 0 {
		crawStartTime := config.Get("CrawlStartTime").(string)
		lastPubTime, _ = time.ParseInLocation("2006-01-02 15:04:05 +0800 CST", crawStartTime, time.Local)
	} else {
		lastPubTime, _ = time.ParseInLocation("2006-01-02 15:04:05 +0800 CST", string(b), time.Local)
	}
}

func ensureDir(str_dir string) (err error) {
	dir, err := os.Stat(str_dir)
	if dir == nil {
		err = os.Mkdir(str_dir, 0777)
	}

	return err
}
