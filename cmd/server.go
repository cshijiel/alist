package cmd

import (
	"context"
	"fmt"
	"github.com/alist-org/alist/v3/internal/db"
	"github.com/alist-org/alist/v3/internal/fs"
	json "github.com/json-iterator/go"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/alist-org/alist/v3/cmd/flags"
	_ "github.com/alist-org/alist/v3/drivers"
	"github.com/alist-org/alist/v3/internal/bootstrap"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/server"

	model2 "github.com/alist-org/alist/v3/internal/model"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// ServerCmd represents the server command
var ServerCmd = &cobra.Command{
	Use:   "server",
	Short: "Start the server at the specified address",
	Long: `Start the server at the specified address
the address is defined in config file`,
	Run: func(cmd *cobra.Command, args []string) {
		Init()
		if conf.Conf.DelayedStart != 0 {
			utils.Log.Infof("delayed start for %d seconds", conf.Conf.DelayedStart)
			time.Sleep(time.Duration(conf.Conf.DelayedStart) * time.Second)
		}
		bootstrap.InitAria2()
		bootstrap.InitQbittorrent()
		bootstrap.LoadStorages()
		if !flags.Debug && !flags.Dev {
			gin.SetMode(gin.ReleaseMode)
		}
		r := gin.New()
		r.Use(gin.LoggerWithWriter(log.StandardLogger().Out), gin.RecoveryWithWriter(log.StandardLogger().Out))
		server.Init(r)
		walkForVideos()
		var httpSrv, httpsSrv, unixSrv *http.Server
		if conf.Conf.Scheme.HttpPort != -1 {
			httpBase := fmt.Sprintf("%s:%d", conf.Conf.Scheme.Address, conf.Conf.Scheme.HttpPort)
			utils.Log.Infof("start HTTP server @ %s", httpBase)
			httpSrv = &http.Server{Addr: httpBase, Handler: r}
			go func() {
				err := httpSrv.ListenAndServe()
				if err != nil && err != http.ErrServerClosed {
					utils.Log.Fatalf("failed to start http: %s", err.Error())
				}
			}()
		}
		if conf.Conf.Scheme.HttpsPort != -1 {
			httpsBase := fmt.Sprintf("%s:%d", conf.Conf.Scheme.Address, conf.Conf.Scheme.HttpsPort)
			utils.Log.Infof("start HTTPS server @ %s", httpsBase)
			httpsSrv = &http.Server{Addr: httpsBase, Handler: r}
			go func() {
				err := httpsSrv.ListenAndServeTLS(conf.Conf.Scheme.CertFile, conf.Conf.Scheme.KeyFile)
				if err != nil && err != http.ErrServerClosed {
					utils.Log.Fatalf("failed to start https: %s", err.Error())
				}
			}()
		}
		if conf.Conf.Scheme.UnixFile != "" {
			utils.Log.Infof("start unix server @ %s", conf.Conf.Scheme.UnixFile)
			unixSrv = &http.Server{Handler: r}
			go func() {
				listener, err := net.Listen("unix", conf.Conf.Scheme.UnixFile)
				if err != nil {
					utils.Log.Fatalf("failed to listen unix: %+v", err)
				}
				// set socket file permission
				mode, err := strconv.ParseUint(conf.Conf.Scheme.UnixFilePerm, 8, 32)
				if err != nil {
					utils.Log.Errorf("failed to parse socket file permission: %+v", err)
				} else {
					err = os.Chmod(conf.Conf.Scheme.UnixFile, os.FileMode(mode))
					if err != nil {
						utils.Log.Errorf("failed to chmod socket file: %+v", err)
					}
				}
				err = unixSrv.Serve(listener)
				if err != nil && err != http.ErrServerClosed {
					utils.Log.Fatalf("failed to start unix: %s", err.Error())
				}
			}()
		}
		// Wait for interrupt signal to gracefully shutdown the server with
		// a timeout of 1 second.
		quit := make(chan os.Signal, 1)
		// kill (no param) default send syscanll.SIGTERM
		// kill -2 is syscall.SIGINT
		// kill -9 is syscall. SIGKILL but can"t be catch, so don't need add it
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		<-quit
		utils.Log.Println("Shutdown server...")
		Release()
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		var wg sync.WaitGroup
		if conf.Conf.Scheme.HttpPort != -1 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := httpSrv.Shutdown(ctx); err != nil {
					utils.Log.Fatal("HTTP server shutdown err: ", err)
				}
			}()
		}
		if conf.Conf.Scheme.HttpsPort != -1 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := httpsSrv.Shutdown(ctx); err != nil {
					utils.Log.Fatal("HTTPS server shutdown err: ", err)
				}
			}()
		}
		if conf.Conf.Scheme.UnixFile != "" {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := unixSrv.Shutdown(ctx); err != nil {
					utils.Log.Fatal("Unix server shutdown err: ", err)
				}
			}()
		}
		wg.Wait()
		utils.Log.Println("Server exit")
	},
}

func walkForVideos() {
	go func() {
		time.Sleep(5 * time.Second)
		log.Infof("开始初始化电影")
		var targetPath = "/Users/didi/temp"
		videos, err := db.GetVideos()
		if err != nil {
			log.Infof("开始初始化电影, err=%v", err)
		}
		for i := range videos {
			node := videos[i]
			filename := node.Name

			var fullFilename = targetPath + node.Parent + "/" + filename

			// 视频，需要302跳转
			if utils.SliceContains([]string{
				"mp4", "mkv", "flv", "avi", "ts", "rm", "rmvb", "webm",
			}, utils.Ext(filename)) {
				var content = map[string]string{
					"FileId":  node.FileId,
					"ShareId": node.ShareId,
				}
				bytes, _ := json.Marshal(content)
				err := os.WriteFile(fullFilename, bytes, 0644)
				if err != nil {
					log.Infof("初始化电影错误, err=%v", err)
				}
				continue
			}

			// 字幕，直接下载
			if utils.SliceContains([]string{
				"ass", "srt", "ssa", "vtt", "idx", "sub", "nfo",
			}, utils.Ext(filename)) {
				link, _, err := fs.Link(context.Background(), node.Parent+"/"+filename, model2.LinkArgs{})
				if err != nil {
					log.Infof("初始化字幕错误, err=%v", err)
					continue
				}
				err = download(link.URL, fullFilename)
				if err != nil {
					log.Infof("初始化字幕错误, err=%v", err)
				}
			}

			// 其他不处理
		}
	}()
}

func download(url string, target string) error {
	// Get the data
	req, _ := http.NewRequest("GET", url, nil)
	// 比如说设置个token
	req.Header.Set("Referer", "https://www.aliyundrive.com/")

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 创建一个文件用于保存
	out, err := utils.CreateNestedFile(target)
	if err != nil {
		return err
	}
	defer out.Close()

	// 然后将响应流和文件流对接起来
	_, err = io.Copy(out, resp.Body)
	return err
}

func init() {
	RootCmd.AddCommand(ServerCmd)

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// serverCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// serverCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}

// OutAlistInit 暴露用于外部启动server的函数
func OutAlistInit() {
	var (
		cmd  *cobra.Command
		args []string
	)
	ServerCmd.Run(cmd, args)
}
