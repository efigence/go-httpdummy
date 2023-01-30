package web

import (
	"fmt"
	"github.com/efigence/go-mon"
	ginzap "github.com/gin-contrib/zap"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var inflightReq = mon.GlobalRegistry.MustRegister("conn.inflight", mon.NewRelativeIntegerGauge())
var errReq = mon.GlobalRegistry.MustRegister("conn.err", mon.NewCounter())
var rateReq = mon.GlobalRegistry.MustRegister("conn.rate", mon.NewEWMARate(time.Second*10))

type WebBackend struct {
	l   *zap.SugaredLogger
	r   *gin.Engine
	cfg *Config
}

type Config struct {
	Logger          *zap.SugaredLogger `yaml:"-"`
	ListenAddr      string             `yaml:"listen_addr"`
	LogHTTPRequests bool               `yaml:"log_http_requests"`
	Code404As200    bool               `yaml:"code_404_as_200"`
}

func New(cfg Config, webFS fs.FS) (backend *WebBackend, err error) {
	if cfg.Logger == nil {
		panic("missing logger")
	}
	if len(cfg.ListenAddr) == 0 {
		panic("missing listen addr")
	}
	w := WebBackend{
		l:   cfg.Logger,
		cfg: &cfg,
	}
	r := gin.New()
	w.r = r
	gin.SetMode(gin.ReleaseMode)
	t, err := template.ParseFS(webFS, "templates/*.tmpl")
	if err != nil {
		return nil, fmt.Errorf("error loading templates: %s", err)
	}
	r.SetHTMLTemplate(t)
	// for zap logging
	if cfg.LogHTTPRequests {
		r.Use(ginzap.Ginzap(w.l.Desugar(), time.RFC3339, false))
	}
	//r.Use(ginzap.RecoveryWithZap(w.l.Desugar(), true))
	// basic logging to stdout
	//r.Use(gin.LoggerWithWriter(os.Stdout))
	r.Use(gin.Recovery())
	r.Use(func(c *gin.Context) {
		rateReq.Update(1)
	})

	// monitoring endpoints
	r.GET("/_status/health", gin.WrapF(mon.HandleHealthcheck))
	r.HEAD("/_status/health", gin.WrapF(mon.HandleHealthcheck))
	r.GET("/_status/metrics", gin.WrapF(mon.HandleMetrics))
	// healthcheckHandler, haproxyStatus := mon.HandleHealthchecksHaproxy()
	// r.GET("/_status/metrics", gin.WrapF(healthcheckHandler))

	httpFS := http.FileServer(http.FS(webFS))
	r.GET("/s/*filepath", func(c *gin.Context) {
		// content is embedded under static/ dir
		p := strings.Replace(c.Request.URL.Path, "/s/", "/static/", -1)
		c.Request.URL.Path = p
		//c.Header("Cache-Control", "public, max-age=3600, immutable")
		httpFS.ServeHTTP(c.Writer, c.Request)
	})
	r.GET("/", func(c *gin.Context) {
		c.HTML(http.StatusOK, "index.tmpl", gin.H{
			"title":      "dummy http backend",
			"RemoteAddr": c.Request.RemoteAddr,
			"IP":         c.ClientIP(),
			"host":       c.Request.Host,
		})
	})
	r.GET("/slow/:duration", w.SlowRequest)
	r.POST("/post", w.PostSink)
	r.POST("/post/:duration", w.PostSink)
	r.NoRoute(func(c *gin.Context) {
		if cfg.Code404As200 {
			c.HTML(http.StatusOK, "404.tmpl", gin.H{
				"notfound": c.Request.URL.Path,
				"msg":      "pretending 404 is 200",
			})
		} else {
			c.HTML(http.StatusNotFound, "404.tmpl", gin.H{
				"notfound": c.Request.URL.Path,
			})
		}
	})
	r.GET("/routes", func(c *gin.Context) {
		c.HTML(http.StatusOK, "routes.tmpl", r.Routes())
	})
	mon.GlobalStatus.Update(mon.Ok, "httptest running")
	return &w, nil
}

func (b *WebBackend) Run() error {
	b.l.Infof("listening on %s", b.cfg.ListenAddr)
	return b.r.Run(b.cfg.ListenAddr)
}

func (b *WebBackend) SlowRequest(c *gin.Context) {
	duration := c.Param("duration")
	interval, err := time.ParseDuration(duration)
	if err != nil {
		c.HTML(
			http.StatusBadRequest, "error.tmpl",
			gin.H{"msg": fmt.Sprintf("bad time: %s", err)})
		return
	}
	inflightReq.Update(1)
	defer inflightReq.Update(-1)
	waitInterval := time.Second * 10
	if interval < waitInterval*10 {
		waitInterval = interval / 10
	}
	c.HTML(http.StatusOK, "slow_pre.tmpl",
		gin.H{
			"duration": interval.String(),
			"interval": waitInterval.String(),
		})
	c.Writer.Flush()
	end := time.After(interval)
	// for longer requests, ping every 10s
	// for shorter requests, take at least 10 packets
	for {
		select {
		case _ = <-end:
			c.HTML(http.StatusOK, "slow_post.tmpl", nil)
			return
		default:
			time.Sleep(waitInterval)
			_, err := c.Writer.Write([]byte(string("!\n"))) // newline because tools like curl linebuffer
			c.Writer.Flush()
			if err != nil {
				errReq.Update(1)
				return
			}
		}
	}
}

func (b *WebBackend) PostSink(c *gin.Context) {
	duration := c.Param("duration")
	interval, err := time.ParseDuration(duration)
	if err != nil && len(duration) > 0 {
		c.HTML(
			http.StatusBadRequest, "error.tmpl",
			gin.H{"msg": fmt.Sprintf("bad time: %s", err)})
		return
	}
	size, err := strconv.Atoi(c.Request.Header.Get("content-length"))
	body := c.Request.Body
	defer body.Close()
	readCtr := 0
	buf := make([]byte, 1024)
	divider := size / (10 * len(buf))
	if divider < 1 {
		divider = 1
	}
	var n int
	err = nil
	// duplicate just for ugly optimization
	if interval.Nanoseconds() > 0 {
		idx := 0
		for err == nil {
			idx++
			n, err = body.Read(buf)
			readCtr = readCtr + n
			if n == 0 {
				break
			}
			if idx%divider == 0 {
				v := []byte(fmt.Sprintf("progress %d/%d\n", readCtr, size))
				c.Writer.Write(v)
				c.Writer.Flush()
			}
			time.Sleep(interval)
		}
	} else {
		for err == nil {
			n, err = body.Read(buf)
			readCtr = readCtr + n
			if n == 0 {
				break
			}
		}
	}

	c.String(http.StatusOK, fmt.Sprintf("received %d bytes\n", readCtr))

}
