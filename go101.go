package main

import (
	"context"
	"go/build"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var (
	rootPath              = findGo101ProjectRoot()
	go101    http.Handler = &Go101{
		staticHandler:     http.StripPrefix("/static/", http.FileServer(http.Dir(filepath.Join(rootPath, "static")))),
		articleResHandler: http.StripPrefix("/article/res/", http.FileServer(http.Dir(filepath.Join(rootPath, "articles", "res")))),
		isLocalServer:     false, // may be modified later
	}
)

type Go101 struct {
	staticHandler     http.Handler
	articleResHandler http.Handler
	isLocalServer     bool
	isLocalServerMu   sync.Mutex
}

func (go101 *Go101) ComfirmLocalServer(isLocal bool) {
	go101.isLocalServerMu.Lock()
	if go101.isLocalServer != isLocal {
		go101.isLocalServer = isLocal
		if go101.isLocalServer {
			go go101.Update()
		}
	}
	go101.isLocalServerMu.Unlock()
}

func (go101 *Go101) IsLocalServer() (isLocal bool) {
	go101.isLocalServerMu.Lock()
	isLocal = go101.isLocalServer
	go101.isLocalServerMu.Unlock()
	return
}

func (go101 *Go101) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	go101.ComfirmLocalServer(isLocalRequest(r))

	group, item := "", ""
	tokens := strings.SplitN(r.URL.Path, "/", 3)
	if len(tokens) > 1 {
		group = tokens[1]
		if len(tokens) > 2 {
			item = tokens[2]
		}
	}

	// log.Println("group=", group, ", item=", item)

	switch strings.ToLower(group) {
	default:
		http.Error(w, "", http.StatusNotFound)
		return
	case "":

	case "static":
		w.Header().Set("Cache-Control", "max-age=360000") // 100 hours // 31536000
		go101.staticHandler.ServeHTTP(w, r)
		return
	case "article":
		item = strings.ToLower(item)
		if strings.HasPrefix(item, "res/") {
			w.Header().Set("Cache-Control", "max-age=360000") // 100 hours // 31536000
			go101.articleResHandler.ServeHTTP(w, r)
			return
		}

		if go101.renderArticlePage(w, r, item) {
			return
		}
	}

	http.Redirect(w, r, "/article/101.html", http.StatusTemporaryRedirect)
}

//===================================================
// pages
//==================================================

type Article struct {
	Content, Title     template.HTML
	TitleWithoutTags   string
	FilenameWithoutExt string
}

var articleContents = func() map[string]Article {
	path := filepath.ToSlash(rootPath + "/articles/")
	if files, err := filepath.Glob(path + "*.html"); err != nil {
		log.Fatal(err)
		return nil
	} else {
		contents := make(map[string]Article, len(files))
		for _, f := range files {
			file, _ := filepath.Rel(path, f)
			contents[file] = Article{}
		}
		return contents
	}
}()

func retrieveArticleContent(file string, cachedIt bool) (Article, error) {
	article, present := articleContents[file]
	if !present {
		return Article{}, nil
	}
	if article.Content == "" {
		content, err := ioutil.ReadFile(filepath.Join(rootPath, "articles", file))
		if err != nil {
			return Article{}, err
		}
		article.Content = template.HTML(content)
		article.FilenameWithoutExt = strings.TrimSuffix(file, ".html")
		retrieveTitlesForArticle(&article)
		if cachedIt {
			articleContents[file] = article
		}
	}
	return article, nil
}

const H1, _H1, MaxLen = "<h1>", "</h1>", 128

var TagSigns = [2]rune{'<', '>'}

func retrieveTitlesForArticle(article *Article) {
	i := strings.Index(string(article.Content), H1)
	if i >= 0 {
		i += len(H1)
		j := strings.Index(string(article.Content[i:i+MaxLen]), _H1)
		if j >= 0 {
			article.Title = article.Content[i-len(H1) : i+j+len(_H1)]
			article.Content = article.Content[i+j+len(_H1):]
			k, s := 0, make([]rune, 0, MaxLen)
			for _, r := range article.Title {
				if r == TagSigns[k] {
					k = (k + 1) & 1
				} else if k == 0 {
					s = append(s, r)
				}
			}
			article.TitleWithoutTags = string(s)
		}
	}
}

func (go101 *Go101) renderArticlePage(w http.ResponseWriter, r *http.Request, file string) bool {
	isLocal := go101.IsLocalServer()
	article, err := retrieveArticleContent(file, !isLocal)
	if err == nil {
		if isLocal {
			w.Header().Set("Cache-Control", "no-cache, private, max-age=0")
		} else {
			w.Header().Set("Cache-Control", "max-age=36000") // 10 hours
		}
		page := map[string]interface{}{
			"Article":       article,
			"Title":         article.TitleWithoutTags,
			"IsLocalServer": isLocal,
		}
		if err = retrievePageTemplate(Template_Article, !isLocal).Execute(w, page); err == nil {
			return true
		}
	}

	w.Header().Set("Cache-Control", "no-cache, private, max-age=0")
	w.Write([]byte(err.Error()))
	return false
}

//===================================================
// tempaltes
//==================================================

type PageTemplate uint

const (
	Template_Article PageTemplate = iota
	NumPageTemplates
)

var pageTemplates [NumPageTemplates + 1]*template.Template

func init() {
	for i := range pageTemplates {
		retrievePageTemplate(PageTemplate(i), false) // must all templates
	}
}

func retrievePageTemplate(which PageTemplate, cacheIt bool) *template.Template {
	if which > NumPageTemplates {
		which = NumPageTemplates
	}
	t := pageTemplates[which]
	if t == nil {
		switch which {
		case Template_Article:
			t = parseTemplate(filepath.Join(rootPath, "templates"), "base", "article")
		default:
			t = template.New("blank")
		}

		if cacheIt {
			pageTemplates[which] = t
		}
	}
	return t
}

//===================================================
// git
//===================================================

func gitPull() ([]byte, error) {
	output, err := runShellCommand(time.Minute/2, "git", "pull")
	if err != nil {
		log.Println("git pull:", err)
	} else {
		log.Printf("git pull: %s", output)
	}
	return output, err
}

func (go101 *Go101) Update() {
	<-time.After(time.Minute * 5)
	gitPull()
	for {
		<-time.After(time.Hour * 24)
		gitPull()
	}
}

//===================================================
// utils
//===================================================

func parseTemplate(path string, files ...string) *template.Template {
	ts := make([]string, len(files))
	for i, f := range files {
		ts[i] = filepath.Join(path, f)
	}
	return template.Must(template.ParseFiles(ts...))
}

// https://stackoverflow.com/questions/39320371/how-start-web-server-to-open-page-in-browser-in-golang
func openBrowser(url string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "windows":
		cmd = "cmd"
		args = []string{"/c", "start"}
	case "darwin":
		cmd = "open"
	default: // "linux", "freebsd", "openbsd", "netbsd"
		cmd = "xdg-open"
	}
	return exec.Command(cmd, append(args, url)...).Start()
}

func findGo101ProjectRoot() string {
	if _, err := os.Stat(filepath.Join(".", "go101.go")); err == nil {
		return "."
	}

	pkg, err := build.Import("github.com/go101/go101", "", build.FindOnly)
	if err != nil {
		log.Fatal("Can't find pacakge: github.com/go101/go101")
		return "."
	}
	return pkg.Dir
}

func isLocalRequest(r *http.Request) bool {
	end := strings.Index(r.Host, ":")
	if end < 0 {
		end = len(r.Host)
	}
	hostname := r.Host[:end]
	return hostname == "localhost" // || hostname == "127.0.0.1" // 127.* for local cached version now
}

func runShellCommand(timeout time.Duration, cmd string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, cmd, args...)
	command.Dir = rootPath
	return command.Output()
}
