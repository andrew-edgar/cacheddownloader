package cacheddownloader

import (
	"bytes"
	"crypto/md5"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const MAX_DOWNLOAD_ATTEMPTS = 3

type DownloadCancelledError struct {
	source   string
	duration time.Duration
	written  int64
}

func NewDownloadCancelledError(source string, duration time.Duration, written int64) error {
	return &DownloadCancelledError{
		source:   source,
		duration: duration,
		written:  written,
	}
}

func (e *DownloadCancelledError) Error() string {
	return fmt.Sprintf("Download cancelled: source '%s', duration '%s', bytes '%d'", e.source, e.duration, e.written)
}

type deadlineConn struct {
	Timeout time.Duration
	net.Conn
}

func (c *deadlineConn) Read(b []byte) (n int, err error) {
	if err = c.Conn.SetDeadline(time.Now().Add(c.Timeout)); err != nil {
		return
	}
	return c.Conn.Read(b)
}

func (c *deadlineConn) Write(b []byte) (n int, err error) {
	if err = c.Conn.SetDeadline(time.Now().Add(c.Timeout)); err != nil {
		return
	}
	return c.Conn.Write(b)
}

type Downloader struct {
	client                    *http.Client
	concurrentDownloadBarrier chan struct{}
}

func NewDownloader(timeout time.Duration, maxConcurrentDownloads int, skipSSLVerification bool) *Downloader {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		Dial: func(netw, addr string) (net.Conn, error) {
			c, err := net.DialTimeout(netw, addr, 10*time.Second)
			if err != nil {
				return nil, err
			}
			if tc, ok := c.(*net.TCPConn); ok {
				tc.SetKeepAlive(true)
				tc.SetKeepAlivePeriod(30 * time.Second)
			}
			return &deadlineConn{5 * time.Second, c}, nil
		},
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: skipSSLVerification,
			MinVersion:         tls.VersionTLS10,
		},
		DisableKeepAlives: true,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}

	return &Downloader{
		client: client,
		concurrentDownloadBarrier: make(chan struct{}, maxConcurrentDownloads),
	}
}

func (downloader *Downloader) Download(
	url *url.URL,
	createDestination func() (*os.File, error),
	cachingInfoIn CachingInfoType,
	cancelChan <-chan struct{},
) (path string, cachingInfoOut CachingInfoType, err error) {

	startTime := time.Now()

	select {
	case downloader.concurrentDownloadBarrier <- struct{}{}:
	case <-cancelChan:
		return "", CachingInfoType{}, NewDownloadCancelledError("download-barrier", time.Now().Sub(startTime), -1)
	}

	defer func() {
		<-downloader.concurrentDownloadBarrier
	}()

	for attempt := 0; attempt < MAX_DOWNLOAD_ATTEMPTS; attempt++ {
		path, cachingInfoOut, err = downloader.fetchToFile(url, createDestination, cachingInfoIn, cancelChan)

		if err == nil {
			break
		}

		if _, ok := err.(*DownloadCancelledError); ok {
			break
		}
	}

	if err != nil {
		return "", CachingInfoType{}, err
	}

	return
}

func (downloader *Downloader) fetchToFile(
	url *url.URL,
	createDestination func() (*os.File, error),
	cachingInfoIn CachingInfoType,
	cancelChan <-chan struct{},
) (string, CachingInfoType, error) {
	var req *http.Request
	var err error

	req, err = http.NewRequest("GET", url.String(), nil)
	if err != nil {
		return "", CachingInfoType{}, err
	}

	if cachingInfoIn.ETag != "" {
		req.Header.Add("If-None-Match", cachingInfoIn.ETag)
	}
	if cachingInfoIn.LastModified != "" {
		req.Header.Add("If-Modified-Since", cachingInfoIn.LastModified)
	}

	completeChan := make(chan struct{})
	defer close(completeChan)

	if transport, ok := downloader.client.Transport.(*http.Transport); ok {
		go func() {
			select {
			case <-completeChan:
			case <-cancelChan:
				transport.CancelRequest(req)
			}
		}()
	}

	startTime := time.Now()

	var resp *http.Response
	resp, err = downloader.client.Do(req)
	if err != nil {
		select {
		case <-cancelChan:
			err = NewDownloadCancelledError("fetch-request", time.Now().Sub(startTime), -1)
		default:
		}
		return "", CachingInfoType{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return "", CachingInfoType{}, nil
	}

	if resp.StatusCode != http.StatusOK {
		return "", CachingInfoType{}, fmt.Errorf("Download failed: Status code %d", resp.StatusCode)
	}

	var destinationFile *os.File
	destinationFile, err = createDestination()
	if err != nil {
		return "", CachingInfoType{}, err
	}

	defer func() {
		destinationFile.Close()
		if err != nil {
			os.Remove(destinationFile.Name())
		}
	}()

	_, err = destinationFile.Seek(0, 0)
	if err != nil {
		return "", CachingInfoType{}, err
	}

	err = destinationFile.Truncate(0)
	if err != nil {
		return "", CachingInfoType{}, err
	}

	go func() {
		select {
		case <-completeChan:
		case <-cancelChan:
			resp.Body.Close()
		}
	}()

	hash := md5.New()

	startTime = time.Now()

	written, err := io.Copy(io.MultiWriter(destinationFile, hash), resp.Body)
	if err != nil {
		select {
		case <-cancelChan:
			err = NewDownloadCancelledError("copy-body", time.Now().Sub(startTime), written)
		default:
		}
		return "", CachingInfoType{}, err
	}

	cachingInfoOut := CachingInfoType{
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	}

	etagChecksum, ok := convertETagToChecksum(cachingInfoOut.ETag)

	if ok && !bytes.Equal(etagChecksum, hash.Sum(nil)) {
		err = fmt.Errorf("Download failed: Checksum mismatch")
		return "", CachingInfoType{}, err
	}

	return destinationFile.Name(), cachingInfoOut, nil
}

// convertETagToChecksum returns true if ETag is a valid MD5 hash, so a checksum action was intended.
// See here for our motivation: http://docs.aws.amazon.com/AmazonS3/latest/API/RESTCommonResponseHeaders.html
func convertETagToChecksum(etag string) ([]byte, bool) {
	etag = strings.Trim(etag, `"`)

	if len(etag) != 32 {
		return nil, false
	}

	c, err := hex.DecodeString(etag)
	if err != nil {
		return nil, false
	}

	return c, true
}
