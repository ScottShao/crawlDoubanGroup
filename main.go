package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

var topicList map[string]*Topic
var config Config
var crawlUrl, crawlUser, jsonPath string
var lastPubTime time.Time //爬过的最后回复时间

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

func init() {
	config = ReadConf("./config.json")
	crawlUrl = config.Get("Url").(string)
	crawlUser = config.Get("UserName").(string)

	if crawlUrl == "" || crawlUser == "" {
		panic("Url or Username is empty")
	}

	jsonPath = filepath.Join("./", crawlUser)
	today := today()
	b, err := ioutil.ReadFile(filepath.Join(jsonPath, "/", today, ".json"))
	if len(b) == 0 {
		b = []byte("{}")
	}
	err = json.Unmarshal(b, &topicList)
	if err != nil {
		panic(err)
	}

	readLastCrawlTime()
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
	fmt.Println("listening on:" + port)
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
	default:
		resp.Write([]byte("no new"))
	}
}

func respTopics(date string, resp http.ResponseWriter) {
	topicList := make(map[string]*Topic, 0)

	jsonPath = filepath.Join("./", crawlUser)
	b, err := ioutil.ReadFile(filepath.Join(jsonPath, date+".json"))
	if len(b) == 0 {
		b = []byte("{}")
	}

	err = json.Unmarshal(b, &topicList)
	if err != nil {
		fmt.Println(err)
		resp.Write([]byte("inner error"))
		return
	}

	if len(topicList) > 0 {
		html := renderHtml(topicList)
		resp.Header().Set("Content", "txt/html")
		resp.Write([]byte(html))
	} else {
		resp.Write([]byte("no new"))
	}
}

func renderHtml(topicList map[string]*Topic) string {
	htmlHead := `<!DOCTYPE html>
		<html>
			<title>topic list</title>
		`
	htmlList := ""
	for _, v := range topicList {
		listHead := fmt.Sprintf(
			`<li>
				<a href=%s style='text-decoration: none;' target=\"blank\">%s -- %s</a>
				<ul>`,
			v.Url, v.Title, v.User)
		listBody := ""
		for _, reply := range v.Replys {
			listBody += fmt.Sprintf(
				`<li>
					<quote>
						%s
					</quote>
					<h3>
						%s
					</h3>
				</li>`, reply.Quote, reply.Content)
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

func startCrawl() {
	go func() {
		runtime.Gosched()

		err := crawlAndThen()
		if err != nil {
			fmt.Println(err)
		}

		for {
			select {
			case <-time.NewTicker(10 * time.Minute).C:
				err := crawlAndThen()
				if err != nil {
					fmt.Println(err)
				}
			}
		}
	}()
}

func crawlAndThen() error {
	hasNew := crawl()
	if !hasNew {
		fmt.Println("no new")
		return nil
	}

	return save()
}

func crawl() bool {
	hasNew := false
	doc, err := goquery.NewDocument(crawlUrl)
	if err != nil {
		fmt.Println(err)
		return false
	}

	topics_root := doc.Find(".olt").First()

	if topics_root == nil {
		return false
	}

	topic_nodes := topics_root.Find("tr")
	topic_nodes = topic_nodes.Slice(1, len(topic_nodes.Nodes))
	page_num := topic_nodes.Length()

	this_crawl_last_time := lastPubTime

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

		docNew, err := goquery.NewDocument(topic_url)
		if err != nil {
			fmt.Println(err)
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
			docNew, err := goquery.NewDocument(topic_url + "?start=" + strconv.Itoa(k*100))
			if err != nil {
				fmt.Println(err)
				return
			}

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
					fmt.Println(err)
					return
				}

				if this_time.After(this_crawl_last_time) {
					this_crawl_last_time = this_time
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
					hasNew = true
				}
			})
			fmt.Println(fmt.Sprintf("crawl reply %d/%d", k+1, reply_page_num))
			time.Sleep(time.Second * 1)
		}

		fmt.Println(fmt.Sprintf("crawl topic %d/%d", i+1, page_num))
		time.Sleep(time.Second * 1)
	})

	lastPubTime = this_crawl_last_time
	return hasNew
}

func today() string {
	year, month, day := time.Now().Date()
	return fmt.Sprintf("%d-%02d-%02d", year, int(month), day)
}

func yesterday() string {
	year, month, day := time.Now().Add(-24 * time.Hour).Date()
	return fmt.Sprintf("%d-%02d-%02d", year, int(month), day)
}

func save() (err error) {
	err = writeLastCrawlTime()
	err = saveTopicList()

	return err
}

func saveTopicList() error {
	dir := filepath.Join("./", crawlUser)
	err := ensureDir(dir)
	if err != nil {
		fmt.Println(err)
		return err
	}

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
