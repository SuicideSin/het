package indexer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"

	"io/ioutil"

	"search/het"

	"code.google.com/p/go.net/html"
	"github.com/boltdb/bolt"
)

func AddOutgoingLink(links *bolt.Bucket, parentLink, childLink het.Link) {
	if parentLink.Outgoing == nil {
		parentLink.Outgoing = make(map[string]bool)
	}

	if childLink.Incomming == nil {
		childLink.Incomming = make(map[string]bool)
	}

	parentLink.Outgoing[childLink.URL.String()] = true
	childLink.Incomming[parentLink.URL.String()] = true

	pbytes, err := json.Marshal(&parentLink)
	if err != nil {
		log.Fatal(err)
	}

	cbytes, err := json.Marshal(&childLink)
	if err != nil {
		log.Fatal(err)
	}

	links.Put([]byte(childLink.URL.String()), cbytes)
	links.Put([]byte(parentLink.URL.String()), pbytes)
}

func GetLink(links *bolt.Bucket, url *url.URL) (het.Link, error) {
	url.Fragment = ""

	lbytes := links.Get([]byte(url.String()))
	link := het.Link{}
	if lbytes != nil {
		// link already exists, return early
		json.Unmarshal(lbytes, &link)

		// follow redirects in the links bucket
		if link.Redirect {
			return GetLink(links, &link.URL)
		}

		return link, nil
	}

	resp, err := http.Get(url.String())
	if err != nil {
		return link, err
	}

	defer resp.Body.Close()

	finalURL := resp.Request.URL
	finalURL.Fragment = ""

	link = het.Link{
		URL:          *finalURL,
		StatusCode:   resp.StatusCode,
		ContentType:  resp.Header.Get("Content-Type"),
		LastModified: strings.Trim(resp.Header.Get("Last-Modified"), " \t\n"),
	}

	lbytes, err = json.Marshal(&link)
	if err != nil {
		log.Fatal(err)
	}

	links.Put([]byte(finalURL.String()), lbytes)

	// redirect link
	if finalURL.String() != url.String() {
		lrbytes, err := json.Marshal(&het.Link{
			URL:      *finalURL,
			Redirect: true,
		})

		if err != nil {
			log.Fatal(err)
		}

		links.Put([]byte(url.String()), lrbytes)
	}

	return link, nil

}

func ValidLink(link het.Link) bool {
	if !(link.URL.Scheme == "http" || link.URL.Scheme == "https") {
		fmt.Printf("ignoring url with unknows scheme %s \n", link.URL.Scheme)
		return false
	}

	if link.StatusCode < 200 || link.StatusCode >= 299 {
		fmt.Printf("page %s not found \n", link.URL.String())
		return false
	}

	if !(strings.Contains(link.ContentType, "html")) {
		fmt.Printf("non html file (%s) ... ignoring\n", link.ContentType)
		return false
	}

	return true
}

func CrawlPage(db *bolt.DB) (het.CountStats, error) {
	countStats := het.CountStats{}
	err := db.Update(func(tx *bolt.Tx) error {
		fmt.Println("Indexing pages ...")

		pending := tx.Bucket([]byte("pending"))
		docs := tx.Bucket([]byte("docs"))
		docKeywords := tx.Bucket([]byte("doc-keywords"))
		links := tx.Bucket([]byte("links"))

		keywords := tx.Bucket([]byte("keywords"))
		stats := tx.Bucket([]byte("stats"))

		cbytes := stats.Get([]byte("count"))
		if cbytes == nil {
			log.Fatal(errors.New("Count Statistics not found in the db!"))
		}

		err := json.Unmarshal(cbytes, &countStats)
		if err != nil {
			log.Fatal(err)
		}

		ubytes, _ := pending.Cursor().First()
		if ubytes == nil {
			fmt.Printf("no pending doc to index ... \n")

			pending.Put([]byte("http://en.wikipedia.org/wiki/List_of_most_popular_websites"), []byte(""))

			// status one means finished, we saturated the internet
			return errors.New("Somehow saturated the entire internet ?!!")
		}

		// delete the url from pending
		pending.Delete(ubytes)

		uri, err := url.Parse(string(ubytes[:]))

		if err != nil {
			fmt.Printf("Cannot parse pending url %s \n", string(ubytes[:]))

			return nil
		}

		link, err := GetLink(links, uri)

		if err != nil {
			// add the page back to pending to try again
			pending.Put([]byte(uri.String()), []byte(""))

			return errors.New("Cannot connect to internet to from link, returning ... ")
		}

		if !ValidLink(link) {
			return nil
		}

		// original uri already indexed
		if docs.Get([]byte(link.URL.String())) != nil {
			// fmt.Printf("uri %s already exists ... ignoring\n", link.URL.String())
			return nil
		}

		resp, err := http.Get(link.URL.String())
		if err != nil {
			// add the page back to pending to try again
			pending.Put([]byte(link.URL.String()), []byte(""))
			return err
		}

		defer resp.Body.Close()

		buff, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			fmt.Printf("cannot read all of html body\n")
			return nil
		}

		contentSize := len(buff)

		htmlRoot, err := html.Parse(bytes.NewReader(buff))
		if err != nil {
			fmt.Printf("got back error parsing html ... ignoring\n")
			return nil
		}

		outgoing := []string{}
		text := []string{}

		var title string

		var f func(*html.Node)
		f = func(n *html.Node) {
			if n.Type == html.ElementNode {
				if n.Data == "a" {
					for _, a := range n.Attr {
						if a.Key == "href" {
							childURL, err := link.URL.Parse(a.Val)
							if err != nil || !(childURL.Scheme == "http" || childURL.Scheme == "https") {
								fmt.Printf("got back error parsing %s\n", a.Val)
								break
							}

							childLink, err := GetLink(links, childURL)

							if err != nil {
								fmt.Printf("Somehow got unlucky and unable to get child link, ignoring: %s\n", err.Error())
								break
							}

							if !ValidLink(childLink) {
								break
							}

							AddOutgoingLink(links, link, childLink)

							outgoing = append(outgoing, childLink.URL.String())
						}
					}
				}

				if n.Data == "title" {
					// get the first text node inside title
					for c := n.FirstChild; c != nil; c = c.NextSibling {
						if c.Type == html.TextNode {
							title = c.Data
							break
						}
					}
				}

				// ignore scripts, styles
				if n.Data == "script" || n.Data == "style" {
					return
				}
			}

			if n.Type == html.TextNode {
				text = append(text, n.Data)
				return
			}

			for c := n.FirstChild; c != nil; c = c.NextSibling {
				f(c)
			}
		}

		f(htmlRoot)

		body := strings.Join(text, "")

		wordCount, length := GetVector(body)

		dockeys := []het.KeywordRef{}
		doc := het.Document{
			URL:    link.URL,
			Title:  title,
			Size:   contentSize,
			Length: length,
		}

		for word := range wordCount {
			if wordCount[word] == 0 {
				continue
			}

			dockeys = append(dockeys, het.KeywordRef{
				Word:      word,
				Frequency: wordCount[word],
			})

			keyword := het.Keyword{
				Frequency: 0,

				Docs: []het.DocumentRef{},
			}

			kbytes := keywords.Get([]byte(word))
			if kbytes != nil {
				json.Unmarshal(kbytes, &keyword)
			} else {
				// a new keyword count, update stats
				countStats.KeywordCount = countStats.KeywordCount + 1
			}

			keyword.Frequency = keyword.Frequency + wordCount[word]

			keyword.Docs = append(keyword.Docs, het.DocumentRef{
				URL:       link.URL,
				Frequency: wordCount[word],
			})

			kbytes, _ = json.Marshal(&keyword)

			keywords.Put([]byte(word), kbytes)
		}

		for _, l := range outgoing {
			countStats.PendingCount = countStats.PendingCount + 1
			pending.Put([]byte(l), []byte(""))
		}

		dbytes, _ := json.Marshal(&doc)
		kbytes, _ := json.Marshal(&dockeys)

		docs.Put([]byte(link.URL.String()), dbytes)
		docKeywords.Put([]byte(link.URL.String()), kbytes)

		countStats.DocumentCount = countStats.DocumentCount + 1

		sbytes, err := json.Marshal(&countStats)
		if err != nil {
			log.Fatal(err)
		}

		stats.Put([]byte("count"), sbytes)

		fmt.Printf("---------------------------------------------\n")
		fmt.Printf("Title         : %s \n", doc.Title)
		fmt.Printf("Url           : %s \n", link.URL.String())
		fmt.Printf("Size          : %d \n", doc.Size)
		fmt.Printf("Last Modified : %s \n", link.LastModified)
		fmt.Printf("Children      : %d \n \n", len(outgoing))

		fmt.Printf("Documents Indexed : %d \n", countStats.DocumentCount)
		fmt.Printf("Documents Left    : %d \n", countStats.PendingCount)
		fmt.Printf("Keywords Indexed  : %d \n", countStats.KeywordCount)

		return nil
	})

	return countStats, err
}