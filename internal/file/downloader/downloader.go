// Copyright (c) 2020 tickstep.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package downloader

import (
	"context"
	"errors"
	"github.com/tickstep/aliyunpan-api/aliyunpan"
	"github.com/tickstep/aliyunpan-api/aliyunpan/apierror"
	"github.com/tickstep/aliyunpan/cmder/cmdutil"
	"github.com/tickstep/aliyunpan/internal/config"
	"github.com/tickstep/aliyunpan/internal/global"
	"github.com/tickstep/aliyunpan/internal/waitgroup"
	"github.com/tickstep/aliyunpan/library/requester/transfer"
	"github.com/tickstep/library-go/cachepool"
	"github.com/tickstep/library-go/logger"
	"github.com/tickstep/library-go/prealloc"
	"github.com/tickstep/library-go/requester"
	"github.com/tickstep/library-go/requester/rio/speeds"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultAcceptRanges 默认的 Accept-Ranges
	DefaultAcceptRanges = "bytes"
)

type (
	// Downloader 下载
	Downloader struct {
		onExecuteEvent        requester.Event    //开始下载事件
		onSuccessEvent        requester.Event    //成功下载事件
		onFailedEvent         requester.Event    //成功下载事件
		onFinishEvent         requester.Event    //结束下载事件
		onPauseEvent          requester.Event    //暂停下载事件
		onResumeEvent         requester.Event    //恢复下载事件
		onCancelEvent         requester.Event    //取消下载事件
		onDownloadStatusEvent DownloadStatusFunc //状态处理事件

		monitorCancelFunc context.CancelFunc
		globalSpeedsStat  *speeds.Speeds // 全局速度统计

		filePanSource           global.FileSourceType // 要下载的网盘文件来源
		fileInfo                *aliyunpan.FileEntity // 下载的文件信息
		driveId                 string
		loadBalancerCompareFunc LoadBalancerCompareFunc // 负载均衡检测函数
		durlCheckFunc           DURLCheckFunc           // 下载url检测函数
		statusCodeBodyCheckFunc StatusCodeBodyCheckFunc
		executeTime             time.Time
		loadBalansers           []string
		writer                  io.WriterAt
		client                  *requester.HTTPClient
		panClient               *config.PanClient
		subPanClientList        []*config.PanClient // 辅助下载子账号列表
		config                  *Config
		monitor                 *Monitor
		instanceState           *InstanceState
	}

	// DURLCheckFunc 下载URL检测函数
	DURLCheckFunc func(client *requester.HTTPClient, durl string) (contentLength int64, resp *http.Response, err error)
	// StatusCodeBodyCheckFunc 响应状态码出错的检查函数
	StatusCodeBodyCheckFunc func(respBody io.Reader) error

	// panClientDownloadUrlEntity 下载url实体，和网盘(用户)客户端绑定
	panClientDownloadUrlEntity struct {
		PanClient *config.PanClient
		FileInfo  *aliyunpan.FileEntity
		DriveId   string
		FileId    string
		FileUrl   string
	}
)

// NewDownloader 初始化Downloader
func NewDownloader(writer io.WriterAt, config *Config, p *config.PanClient, sp []*config.PanClient, globalSpeedsStat *speeds.Speeds) (der *Downloader) {
	der = &Downloader{
		config:           config,
		writer:           writer,
		panClient:        p,
		subPanClientList: sp,
		globalSpeedsStat: globalSpeedsStat,
	}
	return
}

// SetFileInfo 设置文件信息
func (der *Downloader) SetFileInfo(source global.FileSourceType, f *aliyunpan.FileEntity) {
	der.filePanSource = source
	der.fileInfo = f
}
func (der *Downloader) SetDriveId(driveId string) {
	der.driveId = driveId
}

// SetClient 设置http客户端
func (der *Downloader) SetClient(client *requester.HTTPClient) {
	der.client = client
}

// SetLoadBalancerCompareFunc 设置负载均衡检测函数
func (der *Downloader) SetLoadBalancerCompareFunc(f LoadBalancerCompareFunc) {
	der.loadBalancerCompareFunc = f
}

// SetStatusCodeBodyCheckFunc 设置响应状态码出错的检查函数, 当FirstCheckMethod不为HEAD时才有效
func (der *Downloader) SetStatusCodeBodyCheckFunc(f StatusCodeBodyCheckFunc) {
	der.statusCodeBodyCheckFunc = f
}

func (der *Downloader) lazyInit() {
	if der.config == nil {
		der.config = NewConfig()
	}
	if der.client == nil {
		der.client = requester.NewHTTPClient()
		der.client.SetTimeout(20 * time.Minute)
	}
	if der.monitor == nil {
		der.monitor = NewMonitor()
	}
	if der.durlCheckFunc == nil {
		der.durlCheckFunc = DefaultDURLCheckFunc
	}
	if der.loadBalancerCompareFunc == nil {
		der.loadBalancerCompareFunc = DefaultLoadBalancerCompareFunc
	}
}

// SelectParallel 获取合适的 parallel
func (der *Downloader) SelectParallel(prefParallel int, maxParallel int, totalSize int64, instanceRangeList transfer.RangeList) (parallel int) {
	isRange := instanceRangeList != nil && len(instanceRangeList) > 0 // 历史下载分片记录存在
	if prefParallel > 0 {                                             // 单线程下载
		parallel = prefParallel
	} else if isRange {
		parallel = len(instanceRangeList)
	} else {
		parallel = maxParallel                                  // 默认为设置为maxParallel个并发线程数
		if int64(parallel) > totalSize/int64(MinParallelSize) { // 如果文件太小不足切片成maxParallel数量的分片，则计算最接近的分片数量
			parallel = int(totalSize/int64(MinParallelSize)) + 1
		}
	}

	// 其他情况默认使用单线程下载
	if parallel < 1 {
		parallel = 1
	}
	return
}

// SelectBlockSizeAndInitRangeGen 获取合适的 BlockSize, 和初始化 RangeGen
func (der *Downloader) SelectBlockSizeAndInitRangeGen(status *transfer.DownloadStatus, parallel int) (blockSize int64, initErr error) {
	// Range 生成器
	if parallel == 1 { // 单线程
		blockSize = -1
		return
	}
	gen := status.RangeListGen()
	if gen == nil {
		switch der.config.Mode {
		case transfer.RangeGenMode_Default:
			gen = transfer.NewRangeListGenDefault(status.TotalSize(), 0, 0, parallel)
			blockSize = gen.LoadBlockSize()
		case transfer.RangeGenMode_BlockSize:
			b2 := status.TotalSize()/int64(parallel) + 1
			if b2 > der.config.BlockSize { // 选小的BlockSize, 以更高并发
				blockSize = der.config.BlockSize
			} else {
				blockSize = b2
			}

			gen = transfer.NewRangeListGenBlockSize(status.TotalSize(), 0, blockSize)
		default:
			initErr = transfer.ErrUnknownRangeGenMode
			return
		}
	} else {
		blockSize = gen.LoadBlockSize()
	}
	status.SetRangeListGen(gen)
	return
}

// SelectCacheSize 获取合适的 cacheSize
func (der *Downloader) SelectCacheSize(confCacheSize int, blockSize int64) (cacheSize int) {
	if blockSize > 0 && int64(confCacheSize) > blockSize {
		// 如果 cache size 过高, 则调低
		cacheSize = int(blockSize)
	} else {
		cacheSize = confCacheSize
	}
	return
}

// DefaultDURLCheckFunc 默认的 DURLCheckFunc
func DefaultDURLCheckFunc(client *requester.HTTPClient, durl string) (contentLength int64, resp *http.Response, err error) {
	resp, err = client.Req(http.MethodGet, durl, nil, nil)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return 0, nil, err
	}
	return resp.ContentLength, resp, nil
}

func (der *Downloader) checkLoadBalancers() *LoadBalancerResponseList {
	var (
		loadBalancerResponses = make([]*LoadBalancerResponse, 0, len(der.loadBalansers)+1)
		handleLoadBalancer    = func(req *http.Request) {
			if req == nil {
				return
			}

			if der.config.TryHTTP {
				req.URL.Scheme = "http"
			}

			loadBalancer := &LoadBalancerResponse{
				URL: req.URL.String(),
			}

			loadBalancerResponses = append(loadBalancerResponses, loadBalancer)
			logger.Verbosef("DEBUG: load balance task: URL: %s", loadBalancer.URL)
		}
	)

	// 加入第一个
	loadBalancerResponses = append(loadBalancerResponses, &LoadBalancerResponse{
		URL: "der.durl",
	})

	// 负载均衡
	wg := waitgroup.NewWaitGroup(10)
	privTimeout := der.client.Client.Timeout
	der.client.SetTimeout(5 * time.Second)
	for _, loadBalanser := range der.loadBalansers {
		wg.AddDelta()
		go func(loadBalanser string) {
			defer wg.Done()

			subContentLength, subResp, subErr := der.durlCheckFunc(der.client, loadBalanser)
			if subResp != nil {
				subResp.Body.Close() // 不读Body, 马上关闭连接
			}
			if subErr != nil {
				logger.Verbosef("DEBUG: loadBalanser Error: %s\n", subErr)
				return
			}

			// 检测状态码
			switch subResp.StatusCode / 100 {
			case 2: // succeed
			case 4, 5: // error
				var err error
				if der.statusCodeBodyCheckFunc != nil {
					err = der.statusCodeBodyCheckFunc(subResp.Body)
				} else {
					err = errors.New(subResp.Status)
				}
				logger.Verbosef("DEBUG: loadBalanser Status Error: %s\n", err)
				return
			}

			// 检测长度
			if der.fileInfo.FileSize != subContentLength {
				logger.Verbosef("DEBUG: loadBalanser Content-Length not equal to main server\n")
				return
			}

			//if !der.loadBalancerCompareFunc(der.firstInfo.ToMap(), subResp) {
			//	logger.Verbosef("DEBUG: loadBalanser not equal to main server\n")
			//	return
			//}

			handleLoadBalancer(subResp.Request)
		}(loadBalanser)
	}
	wg.Wait()
	der.client.SetTimeout(privTimeout)

	loadBalancerResponseList := NewLoadBalancerResponseList(loadBalancerResponses)
	return loadBalancerResponseList
}

// Execute 开始任务
func (der *Downloader) Execute() error {
	der.lazyInit()

	// zero file, no need to download data
	if der.fileInfo.FileSize == 0 {
		cmdutil.Trigger(der.onFinishEvent)
		return nil
	}

	var (
		loadBalancerResponseList = der.checkLoadBalancers()
		bii                      *transfer.DownloadInstanceInfo
	)

	err := der.initInstanceState(der.config.InstanceStateStorageFormat)
	if err != nil {
		return err
	}
	bii = der.instanceState.Get()

	var (
		isInstance = bii != nil // 是否存在断点信息
		status     *transfer.DownloadStatus
	)
	if !isInstance {
		bii = &transfer.DownloadInstanceInfo{}
	}

	if bii.DownloadStatus != nil {
		// 使用断点信息的状态
		status = bii.DownloadStatus
	} else {
		// 新建状态
		status = transfer.NewDownloadStatus()
		status.SetTotalSize(der.fileInfo.FileSize)
	}

	// 设置限速
	if der.config.MaxRate > 0 {
		rl := speeds.NewRateLimit(der.config.MaxRate)
		status.SetRateLimit(rl)
		defer rl.Stop()
	}

	// 计算本文件下载的并发线程数（分片数）
	parallel := der.SelectParallel(der.config.SliceParallel, MaxParallelWorkerCount, status.TotalSize(), bii.Ranges) // 实际的下载并行量
	blockSize, err := der.SelectBlockSizeAndInitRangeGen(status, parallel)                                           // 实际的BlockSize
	if err != nil {
		return err
	}

	cacheSize := der.SelectCacheSize(der.config.CacheSize, blockSize) // 实际下载缓存
	cachepool.SetSyncPoolSize(cacheSize)                              // 调整pool大小

	logger.Verbosef("DEBUG: download task CREATED: parallel: %d, cache size: %d\n", parallel, cacheSize)

	der.monitor.InitMonitorCapacity(parallel)

	var writer Writer
	// 尝试修剪文件
	if fder, ok := der.writer.(Fder); ok {
		err = prealloc.PreAlloc(fder.Fd(), status.TotalSize())
		if err != nil {
			logger.Verbosef("DEBUG: truncate file error: %s\n", err)
		}
	}
	writer = der.writer

	// 数据平均分配给各个线程
	isRange := bii.Ranges != nil && len(bii.Ranges) > 0
	if !isRange {
		// 没有使用断点续传
		// 分配线程
		bii.Ranges = make(transfer.RangeList, 0, parallel)
		if parallel == 1 { // 单线程
			bii.Ranges = append(bii.Ranges, &transfer.Range{Begin: 0, End: der.fileInfo.FileSize})
		} else {
			gen := status.RangeListGen()
			for i := 0; i < cap(bii.Ranges); i++ {
				_, r := gen.GenRange()
				if r == nil {
					break
				}
				bii.Ranges = append(bii.Ranges, r)
			}
		}
	}

	var (
		writeMu = &sync.Mutex{}
	)

	// 获取各个账号网盘客户端的下载链接
	panClientFileUrl, err := der.getFileAllClientDownloadUrl()
	if err != nil || panClientFileUrl == nil {
		logger.Verbosef("ERROR: get download url error: %s\n", der.fileInfo.FileId)
		cmdutil.Trigger(der.onCancelEvent)
		return err
	}

	// 初始化下载worker
	factorNum := len(bii.Ranges) / len(panClientFileUrl)
	for k, r := range bii.Ranges {
		panClientUrl := panClientFileUrl[int(k/factorNum)]
		loadBalancer := loadBalancerResponseList.SequentialGet()
		if loadBalancer == nil {
			continue
		}

		logger.Verbosef("work id: %d, download url: %v\n", k, panClientUrl.FileUrl)
		client := requester.NewHTTPClient()
		client.SetKeepAlive(true)
		client.SetTimeout(10 * time.Minute)

		realUrl := panClientUrl.FileUrl
		worker := NewWorker(k, panClientUrl.DriveId, panClientUrl.FileInfo.FileId, realUrl, writer, der.globalSpeedsStat)
		worker.SetClient(client)
		worker.SetPanClient(panClientUrl.PanClient)
		worker.SetWriteMutex(writeMu)
		worker.SetTotalSize(der.fileInfo.FileSize)

		worker.SetAcceptRange("bytes")
		worker.SetRange(r) // 分配Range
		der.monitor.Append(worker)
	}

	der.monitor.SetStatus(status)

	// 阿里云盘支持断点续传，开启重载worker
	der.monitor.SetReloadWorker(true)

	moniterCtx, moniterCancelFunc := context.WithCancel(context.Background())
	der.monitorCancelFunc = moniterCancelFunc

	der.monitor.SetInstanceState(der.instanceState)

	// 开始执行
	der.executeTime = time.Now()
	cmdutil.Trigger(der.onExecuteEvent)
	der.downloadStatusEvent() // 启动执行状态处理事件
	der.monitor.Execute(moniterCtx)

	// 检查错误
	err = der.monitor.Err()
	if err == nil { // 成功
		cmdutil.Trigger(der.onSuccessEvent)
		der.removeInstanceState() // 移除断点续传文件
	} else {
		if err == ErrNoWokers && der.fileInfo.FileSize == 0 {
			cmdutil.Trigger(der.onSuccessEvent)
			der.removeInstanceState() // 移除断点续传文件
		}
	}

	// 执行结束
	cmdutil.Trigger(der.onFinishEvent)
	return err
}

// 获取对应网盘文件的下载链接
func (der *Downloader) getFileAllClientDownloadUrl() ([]*panClientDownloadUrlEntity, error) {
	if der.filePanSource == global.AlbumSource {
		// 相册源只支持主账号下载
		return der.getAlbumSourceDownloadUrl()
	} else {
		// 文件源支持多账号分流下载
		return der.getFileSourceDownloadUrl()
	}
}

// getFileSourceDownloadUrl 获取文件源的文件下载链接
func (der *Downloader) getFileSourceDownloadUrl() ([]*panClientDownloadUrlEntity, error) {
	result := []*panClientDownloadUrlEntity{}

	// 主账号（必须存在）
	// 获取下载链接
	var apierr *apierror.ApiError
	durl, apierr := der.panClient.OpenapiPanClient().GetFileDownloadUrl(&aliyunpan.GetFileDownloadUrlParam{
		DriveId: der.driveId,
		FileId:  der.fileInfo.FileId,
	})
	time.Sleep(time.Duration(200) * time.Millisecond)
	if apierr != nil {
		logger.Verbosef("ERROR: get download url error: %s\n", der.fileInfo.FileId)
		cmdutil.Trigger(der.onCancelEvent)
		return nil, apierr
	}
	if durl == nil || durl.Url == "" || strings.HasPrefix(durl.Url, aliyunpan.IllegalDownloadUrlPrefix) {
		logger.Verbosef("无法获取有效的下载链接: %+v\n", durl)
		cmdutil.Trigger(der.onCancelEvent)
		der.removeInstanceState() // 移除断点续传文件
		cmdutil.Trigger(der.onFailedEvent)
		return nil, ErrFileDownloadForbidden
	}
	result = append(result, &panClientDownloadUrlEntity{
		PanClient: der.panClient,
		FileInfo:  der.fileInfo,
		DriveId:   der.driveId,
		FileId:    der.fileInfo.FileId,
		FileUrl:   durl.Url,
	})

	// 网盘名称
	mainDriveName := ""
	openUserInfo, err := der.panClient.OpenapiPanClient().GetUserInfo()
	if err != nil || openUserInfo == nil {
		return nil, err
	}
	if openUserInfo.FileDriveId == der.driveId {
		mainDriveName = "File"
	} else if openUserInfo.ResourceDriveId == der.driveId {
		mainDriveName = "Resource"
	}

	// 网盘文件路径
	mainFileFullPath := der.fileInfo.Path

	// 铺助账号的下载链接，只支持文件源
	if der.subPanClientList != nil {
		for _, spc := range der.subPanClientList {
			driveId := ""
			userInfo, err1 := spc.OpenapiPanClient().GetUserInfo()
			if err1 != nil {
				continue
			}
			if mainDriveName == "File" {
				driveId = userInfo.FileDriveId
			} else if mainDriveName == "Resource" {
				driveId = userInfo.ResourceDriveId
			}

			// 文件信息
			panfileInfo, err2 := spc.OpenapiPanClient().FileInfoByPath(driveId, mainFileFullPath)
			if err2 != nil || panfileInfo == nil {
				continue
			}

			// 下载链接
			durl2, apierr := spc.OpenapiPanClient().GetFileDownloadUrl(&aliyunpan.GetFileDownloadUrlParam{
				DriveId: driveId,
				FileId:  panfileInfo.FileId,
			})
			time.Sleep(time.Duration(200) * time.Millisecond)
			if apierr != nil {
				logger.Verbosef("ERROR: get download url error: %s\n", der.fileInfo.FileId)
				continue
			}
			if durl2 == nil || durl2.Url == "" || strings.HasPrefix(durl2.Url, aliyunpan.IllegalDownloadUrlPrefix) {
				logger.Verbosef("无法获取有效的下载链接: %+v\n", durl2)
				continue
			}
			result = append(result, &panClientDownloadUrlEntity{
				PanClient: spc,
				FileInfo:  panfileInfo,
				DriveId:   driveId,
				FileId:    panfileInfo.FileId,
				FileUrl:   durl2.Url,
			})
		}
	}

	return result, nil
}

// getAlbumSourceDownloadUrl 获取相册源的文件下载链接
func (der *Downloader) getAlbumSourceDownloadUrl() ([]*panClientDownloadUrlEntity, error) {
	result := []*panClientDownloadUrlEntity{}

	// 相册源只能使用主账号
	// 获取下载链接
	var apierr *apierror.ApiError
	durl, apierr := der.panClient.OpenapiPanClient().ShareAlbumGetFileDownloadUrl(&aliyunpan.ShareAlbumGetFileUrlParam{
		AlbumId: der.fileInfo.AlbumId,
		DriveId: der.fileInfo.DriveId,
		FileId:  der.fileInfo.FileId,
	})
	time.Sleep(time.Duration(200) * time.Millisecond)
	if apierr != nil {
		logger.Verbosef("ERROR: get album file download url error: %s\n", der.fileInfo.FileId)
		cmdutil.Trigger(der.onCancelEvent)
		return nil, apierr
	}
	realUrl := ""
	if durl.StreamsUrl != nil { // 实况图片
		if strings.ToLower(der.fileInfo.FileExtension) == "mov" {
			// 视频文件
			if durl.StreamsUrl.Mov != "" {
				realUrl = durl.StreamsUrl.Mov
			}
		} else {
			// 照片文件
			if durl.StreamsUrl.Heic != "" {
				realUrl = durl.StreamsUrl.Heic
			} else if durl.StreamsUrl.Jpeg != "" {
				realUrl = durl.StreamsUrl.Jpeg
			}
		}
	} else if durl.Url != "" { // 普通图片
		realUrl = durl.Url
	}
	if realUrl == "" {
		logger.Verbosef("无法获取有效的相册文件下载链接: %+v\n", durl)
		cmdutil.Trigger(der.onCancelEvent)
		der.removeInstanceState() // 移除断点续传文件
		cmdutil.Trigger(der.onFailedEvent)
		return nil, ErrFileDownloadForbidden
	}
	// 返回下载链接
	result = append(result, &panClientDownloadUrlEntity{
		PanClient: der.panClient,
		FileInfo:  der.fileInfo,
		DriveId:   der.driveId,
		FileId:    der.fileInfo.FileId,
		FileUrl:   realUrl,
	})
	return result, nil
}

// downloadStatusEvent 执行状态处理事件
func (der *Downloader) downloadStatusEvent() {
	if der.onDownloadStatusEvent == nil {
		return
	}

	status := der.monitor.Status()
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-der.monitor.completed:
				return
			case <-ticker.C:
				time.Sleep(500 * time.Millisecond)
				der.onDownloadStatusEvent(status, der.monitor.RangeWorker)
			}
		}
	}()
}

// Pause 暂停
func (der *Downloader) Pause() {
	if der.monitor == nil {
		return
	}
	cmdutil.Trigger(der.onPauseEvent)
	der.monitor.Pause()
}

// Resume 恢复
func (der *Downloader) Resume() {
	if der.monitor == nil {
		return
	}
	cmdutil.Trigger(der.onResumeEvent)
	der.monitor.Resume()
}

// Cancel 取消
func (der *Downloader) Cancel() {
	if der.monitor == nil {
		return
	}
	cmdutil.Trigger(der.onCancelEvent)
	cmdutil.Trigger(der.monitorCancelFunc)
}

// Failed 失败
func (der *Downloader) Failed() {
	if der.monitor == nil {
		return
	}
	cmdutil.Trigger(der.onFailedEvent)
	cmdutil.Trigger(der.monitorCancelFunc)
}

// OnExecute 设置开始下载事件
func (der *Downloader) OnExecute(onExecuteEvent requester.Event) {
	der.onExecuteEvent = onExecuteEvent
}

// OnSuccess 设置成功下载事件
func (der *Downloader) OnSuccess(onSuccessEvent requester.Event) {
	der.onSuccessEvent = onSuccessEvent
}

// OnFailed 设置失败事件
func (der *Downloader) OnFailed(onFailedEvent requester.Event) {
	der.onFailedEvent = onFailedEvent
}

// OnFinish 设置结束下载事件
func (der *Downloader) OnFinish(onFinishEvent requester.Event) {
	der.onFinishEvent = onFinishEvent
}

// OnPause 设置暂停下载事件
func (der *Downloader) OnPause(onPauseEvent requester.Event) {
	der.onPauseEvent = onPauseEvent
}

// OnResume 设置恢复下载事件
func (der *Downloader) OnResume(onResumeEvent requester.Event) {
	der.onResumeEvent = onResumeEvent
}

// OnCancel 设置取消下载事件
func (der *Downloader) OnCancel(onCancelEvent requester.Event) {
	der.onCancelEvent = onCancelEvent
}

// OnDownloadStatusEvent 设置状态处理函数
func (der *Downloader) OnDownloadStatusEvent(f DownloadStatusFunc) {
	der.onDownloadStatusEvent = f
}
