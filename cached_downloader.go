package cacheddownloader

import (
	"crypto/md5"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"sync"
	"time"
)

// called after a new object has entered the cache.
// it is assumed that `path` will be removed, if a new path is returned.
// a noop transformer returns the given path and its detected size.
type CacheTransformer func(source, destination string) (newSize int64, err error)

//go:generate counterfeiter . CachedDownloader

type CachedDownloader interface {
	// Fetch downloads the file at the given URL and stores it in the cache with the given cacheKey.
	// If cacheKey is empty, the file will not be saved in the cache.
	//
	// Fetch returns a stream that can be used to read the contents of the downloaded file. While this stream is active (i.e., not yet closed),
	// the associated cache entry will be considered in use and will not be ejected from the cache.
	Fetch(urlToFetch *url.URL, cacheKey string, cancelChan <-chan struct{}) (stream io.ReadCloser, size int64, err error)

	// FetchAsDirectory downloads the tarfile pointed to by the given URL, expands the tarfile into a directory, and returns the path of that directory.
	FetchAsDirectory(urlToFetch *url.URL, cacheKey string, cancelChan <-chan struct{}) (dirPath string, err error)

	// CloseDirectory decrements the usage counter for the given cacheKey/directoryPath pair.
	// It should be called when the directory returned by FetchAsDirectory is no longer in use.
	// In this way, FetchAsDirectory and CloseDirectory should be treated as a pair of operations,
	// and a process that calls FetchAsDirectory should make sure a corresponding CloseDirectory is eventually called.
	CloseDirectory(cacheKey, directoryPath string) error
}

func NoopTransform(source, destination string) (int64, error) {
	err := replace(source, destination)
	if err != nil {
		return 0, err
	}

	fi, err := os.Stat(destination)
	if err != nil {
		return 0, err
	}

	return fi.Size(), nil
}

type CachingInfoType struct {
	ETag         string
	LastModified string
}

type cachedDownloader struct {
	downloader   *Downloader
	uncachedPath string
	cache        *FileCache
	transformer  CacheTransformer

	lock       *sync.Mutex
	inProgress map[string]chan struct{}
}

func (c CachingInfoType) isCacheable() bool {
	return c.ETag != "" || c.LastModified != ""
}

func (c CachingInfoType) Equal(other CachingInfoType) bool {
	return c.ETag == other.ETag && c.LastModified == other.LastModified
}

// A transformer function can be used to do post-download
// processing on the file before it is stored in the cache.
func New(cachedPath string, uncachedPath string, maxSizeInBytes int64, downloadTimeout time.Duration, maxConcurrentDownloads int, skipSSLVerification bool, transformer CacheTransformer) *cachedDownloader {
	os.RemoveAll(cachedPath)
	os.MkdirAll(cachedPath, 0770)
	return &cachedDownloader{
		downloader:   NewDownloader(downloadTimeout, maxConcurrentDownloads, skipSSLVerification),
		uncachedPath: uncachedPath,
		cache:        NewCache(cachedPath, maxSizeInBytes),
		transformer:  transformer,
		lock:         &sync.Mutex{},
		inProgress:   map[string]chan struct{}{},
	}
}

func (c *cachedDownloader) CloseDirectory(cacheKey, directoryPath string) error {
	cacheKey = fmt.Sprintf("%x", md5.Sum([]byte(cacheKey)))
	return c.cache.CloseDirectory(cacheKey, directoryPath)
}

func (c *cachedDownloader) Fetch(url *url.URL, cacheKey string, cancelChan <-chan struct{}) (io.ReadCloser, int64, error) {
	if cacheKey == "" {
		return c.fetchUncachedFile(url, c.transformer, cancelChan)
	}

	cacheKey = fmt.Sprintf("%x", md5.Sum([]byte(cacheKey)))
	return c.fetchCachedFile(url, cacheKey, cancelChan)
}

func (c *cachedDownloader) fetchUncachedFile(url *url.URL, transformer CacheTransformer, cancelChan <-chan struct{}) (*CachedFile, int64, error) {
	download, _, size, err := c.populateCache(url, "uncached", CachingInfoType{}, transformer, cancelChan)
	if err != nil {
		return nil, 0, err
	}

	file, err := tempFileRemoveOnClose(download.path)
	return file, size, err
}

func (c *cachedDownloader) fetchCachedFile(url *url.URL, cacheKey string, cancelChan <-chan struct{}) (*CachedFile, int64, error) {
	rateLimiter, err := c.acquireLimiter(cacheKey, cancelChan)
	if err != nil {
		return nil, 0, err
	}
	defer c.releaseLimiter(cacheKey, rateLimiter)

	// lookup cache entry
	currentReader, currentCachingInfo, getErr := c.cache.Get(cacheKey)

	// download (short circuits if endpoint respects etag/etc.)
	download, cacheIsWarm, size, err := c.populateCache(url, cacheKey, currentCachingInfo, c.transformer, cancelChan)
	if err != nil {
		if currentReader != nil {
			currentReader.Close()
		}
		return nil, 0, err
	}

	// nothing had to be downloaded; return the cached entry
	if cacheIsWarm {
		return currentReader, 0, getErr
	}

	// current cache is not fresh; disregard it
	if currentReader != nil {
		currentReader.Close()
	}

	// fetch uncached data
	var newReader *CachedFile
	if download.cachingInfo.isCacheable() {
		newReader, err = c.cache.Add(cacheKey, download.path, download.size, download.cachingInfo)
		if err == NotEnoughSpace {
			file, err := tempFileRemoveOnClose(download.path)
			return file, size, err
		}
	} else {
		c.cache.Remove(cacheKey)
		newReader, err = tempFileRemoveOnClose(download.path)
	}

	// return newly fetched file
	return newReader, size, err
}

// Fetch as Directory Code
func (c *cachedDownloader) FetchAsDirectory(url *url.URL, cacheKey string, cancelChan <-chan struct{}) (string, error) {
	cacheKey = fmt.Sprintf("%x", md5.Sum([]byte(cacheKey)))
	return c.fetchCachedDirectory(url, cacheKey, cancelChan)
}

func (c *cachedDownloader) fetchCachedDirectory(url *url.URL, cacheKey string, cancelChan <-chan struct{}) (string, error) {
	rateLimiter, err := c.acquireLimiter(cacheKey, cancelChan)
	if err != nil {
		return "", err
	}
	defer c.releaseLimiter(cacheKey, rateLimiter)

	// lookup cache entry
	currentDirectory, currentCachingInfo, getErr := c.cache.GetDirectory(cacheKey)
	if getErr == NotEnoughSpace {
		// We had a cache hit but cannot expand it in the cache
		return "", getErr
	}

	// download (short circuits if endpoint respects etag/etc.)
	download, cacheIsWarm, _, err := c.populateCache(url, cacheKey, currentCachingInfo, TarTransform, cancelChan)
	if err != nil {
		if currentDirectory != "" {
			c.cache.CloseDirectory(cacheKey, currentDirectory)
		}
		return "", err
	}

	// nothing had to be downloaded; return the cached entry
	if cacheIsWarm {
		return currentDirectory, getErr
	}

	// current cache is not fresh; disregard it
	if currentDirectory != "" {
		c.cache.CloseDirectory(cacheKey, currentDirectory)
	}

	// fetch uncached data
	var newDirectory string
	if download.cachingInfo.isCacheable() {
		newDirectory, err = c.cache.AddDirectory(cacheKey, download.path, download.size, download.cachingInfo)
		if err == NotEnoughSpace {
			return "", err
		}
		// return newly fetched file
		return newDirectory, err
	} else {
		c.cache.Remove(cacheKey)
	}

	return "", NotCacheable
}

func (c *cachedDownloader) acquireLimiter(cacheKey string, cancelChan <-chan struct{}) (chan struct{}, error) {
	startTime := time.Now()

	for {
		c.lock.Lock()
		rateLimiter := c.inProgress[cacheKey]
		if rateLimiter == nil {
			rateLimiter = make(chan struct{})
			c.inProgress[cacheKey] = rateLimiter
			c.lock.Unlock()
			return rateLimiter, nil
		}
		c.lock.Unlock()

		select {
		case <-rateLimiter:
		case <-cancelChan:
			return nil, NewDownloadCancelledError("acquire-limiter", time.Now().Sub(startTime), NoBytesReceived)
		}
	}
}

func (c *cachedDownloader) releaseLimiter(cacheKey string, limiter chan struct{}) {
	c.lock.Lock()
	delete(c.inProgress, cacheKey)
	close(limiter)
	c.lock.Unlock()
}

func tempFileRemoveOnClose(path string) (*CachedFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	return NewFileCloser(f, "", func(path string) {
		os.RemoveAll(path)
	}), nil
}

type download struct {
	path        string
	size        int64
	cachingInfo CachingInfoType
}

func (c *cachedDownloader) populateCache(
	url *url.URL,
	name string,
	cachingInfo CachingInfoType,
	transformer CacheTransformer,
	cancelChan <-chan struct{},
) (download, bool, int64, error) {
	filename, cachingInfo, err := c.downloader.Download(url, func() (*os.File, error) {
		return ioutil.TempFile(c.uncachedPath, name+"-")
	}, cachingInfo, cancelChan)
	if err != nil {
		return download{}, false, 0, err
	}

	if filename == "" {
		return download{}, true, 0, nil
	}

	fileInfo, err := os.Stat(filename)
	if err != nil {
		return download{}, false, 0, err
	}

	cachedFile, err := ioutil.TempFile(c.uncachedPath, "transformed")
	if err != nil {
		return download{}, false, 0, err
	}

	err = cachedFile.Close()
	if err != nil {
		return download{}, false, 0, err
	}

	cachedSize, err := transformer(filename, cachedFile.Name())
	if err != nil {
		// os.Remove(filename)
		return download{}, false, 0, err
	}

	return download{
		path:        cachedFile.Name(),
		size:        cachedSize,
		cachingInfo: cachingInfo,
	}, false, fileInfo.Size(), nil
}
