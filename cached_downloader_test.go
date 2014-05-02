package cacheddownloader_test

import (
	"crypto/md5"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	Url "net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/ghttp"
	"github.com/pivotal-golang/cacheddownloader"
)

func computeMd5(key string) string {
	return fmt.Sprintf("%x", md5.Sum([]byte(key)))
}

var _ = Describe("File cache", func() {
	var (
		cache           cacheddownloader.CachedDownloader
		cachedPath      string
		uncachedPath    string
		maxSizeInBytes  int64
		downloadContent []byte
		url             *Url.URL
		server          *ghttp.Server
	)

	BeforeEach(func() {
		var err error
		cachedPath, err = ioutil.TempDir("", "test_file_cached")
		Ω(err).ShouldNot(HaveOccurred())

		uncachedPath, err = ioutil.TempDir("", "test_file_uncached")
		Ω(err).ShouldNot(HaveOccurred())

		maxSizeInBytes = 1024

		cache = cacheddownloader.New(cachedPath, uncachedPath, maxSizeInBytes, time.Second)
		server = ghttp.NewServer()

		url, err = Url.Parse(server.URL() + "/my_file")
		Ω(err).ShouldNot(HaveOccurred())
	})

	AfterEach(func() {
		os.RemoveAll(cachedPath)
		os.RemoveAll(uncachedPath)
	})

	var (
		file io.ReadCloser
		err  error
	)

	Describe("when the cache folder does not exist", func() {
		It("should create it", func() {
			os.RemoveAll(cachedPath)
			cache = cacheddownloader.New(cachedPath, uncachedPath, maxSizeInBytes, time.Second)
			_, err := os.Stat(cachedPath)
			Ω(err).ShouldNot(HaveOccurred())
		})
	})

	Describe("when the cache folder has stuff in it", func() {
		It("should nuke that stuff", func() {
			filename := filepath.Join(cachedPath, "last_nights_dinner")
			ioutil.WriteFile(filename, []byte("leftovers"), 0666)
			cache = cacheddownloader.New(cachedPath, uncachedPath, maxSizeInBytes, time.Second)
			_, err := os.Stat(filename)
			Ω(err).Should(HaveOccurred())
		})
	})

	Describe("When providing a file that should not be cached", func() {
		Context("when the download succeeds", func() {
			BeforeEach(func() {
				downloadContent = []byte(strings.Repeat("7", int(maxSizeInBytes*3)))

				header := http.Header{}
				header.Set("ETag", "foo")
				server.AppendHandlers(ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/my_file"),
					ghttp.RespondWith(http.StatusOK, string(downloadContent), header),
				))

				file, err = cache.Fetch(url, false)
			})

			It("should not error", func() {
				Ω(err).ShouldNot(HaveOccurred())
			})

			It("should return a readCloser that streams the file", func() {
				Ω(file).ShouldNot(BeNil())
				Ω(ioutil.ReadAll(file)).Should(Equal(downloadContent))
			})

			It("should delete the file when we close the readCloser", func() {
				err := file.Close()
				Ω(err).ShouldNot(HaveOccurred())
				Ω(ioutil.ReadDir(uncachedPath)).Should(HaveLen(0))
			})
		})

		Context("when the download fails", func() {
			BeforeEach(func() {
				server.AllowUnhandledRequests = true //will 500 for any attempted requests
				file, err = cache.Fetch(url, false)
			})

			It("should return an error and no file", func() {
				Ω(file).Should(BeNil())
				Ω(err).Should(HaveOccurred())
			})

			It("should clean up after itself", func() {
				Ω(ioutil.ReadDir(uncachedPath)).Should(HaveLen(0))
			})
		})
	})

	Describe("When providing a file that should be cached", func() {
		var cacheFilePath string
		var returnedHeader http.Header
		BeforeEach(func() {
			cacheFilePath = filepath.Join(cachedPath, computeMd5(url.String()))
			returnedHeader = http.Header{}
			returnedHeader.Set("ETag", "my-original-etag")
		})

		Context("when the file is not in the cache", func() {
			Context("when the download succeeds", func() {
				BeforeEach(func() {
					downloadContent = []byte(strings.Repeat("7", int(maxSizeInBytes/2)))
					server.AppendHandlers(ghttp.CombineHandlers(
						ghttp.VerifyRequest("GET", "/my_file"),
						http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
							Ω(req.Header.Get("If-None-Match")).Should(BeEmpty())
						}),
						ghttp.RespondWith(http.StatusOK, string(downloadContent), returnedHeader),
					))

					file, err = cache.Fetch(url, true)
				})

				It("should not error", func() {
					Ω(err).ShouldNot(HaveOccurred())
				})

				It("should return a readCloser that streams the file", func() {
					Ω(file).ShouldNot(BeNil())
					Ω(ioutil.ReadAll(file)).Should(Equal(downloadContent))
				})

				It("should return a file within the cache", func() {
					Ω(ioutil.ReadDir(cachedPath)).Should(HaveLen(1))
				})

				It("should remove any temporary assets generated along the way", func() {
					Ω(ioutil.ReadDir(uncachedPath)).Should(HaveLen(0))
				})
			})

			Context("when the download succeeds but does not have an ETag", func() {
				BeforeEach(func() {
					downloadContent = []byte(strings.Repeat("7", int(maxSizeInBytes/2)))
					server.AppendHandlers(ghttp.CombineHandlers(
						ghttp.VerifyRequest("GET", "/my_file"),
						http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
							Ω(req.Header.Get("If-None-Match")).Should(BeEmpty())
						}),
						ghttp.RespondWith(http.StatusOK, string(downloadContent)),
					))

					file, err = cache.Fetch(url, true)
				})

				It("should not error", func() {
					Ω(err).ShouldNot(HaveOccurred())
				})

				It("should return a readCloser that streams the file", func() {
					Ω(file).ShouldNot(BeNil())
					Ω(ioutil.ReadAll(file)).Should(Equal(downloadContent))
				})

				It("should not store the file", func() {
					Ω(ioutil.ReadDir(cachedPath)).Should(HaveLen(0))
					Ω(ioutil.ReadDir(uncachedPath)).Should(HaveLen(0))
				})
			})

			Context("when the download fails", func() {
				BeforeEach(func() {
					server.AllowUnhandledRequests = true //will 500 for any attempted requests
					file, err = cache.Fetch(url, true)
				})

				It("should return an error and no file", func() {
					Ω(file).Should(BeNil())
					Ω(err).Should(HaveOccurred())
				})

				It("should clean up after itself", func() {
					Ω(ioutil.ReadDir(cachedPath)).Should(HaveLen(0))
					Ω(ioutil.ReadDir(uncachedPath)).Should(HaveLen(0))
				})
			})
		})

		Context("when the file is already on disk in the cache", func() {
			var fileContent []byte
			var status int
			var downloadContent string

			BeforeEach(func() {
				status = http.StatusOK
				fileContent = []byte("now you see it")

				server.AppendHandlers(ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/my_file"),
					ghttp.RespondWith(http.StatusOK, string(fileContent), returnedHeader),
				))

				cache.Fetch(url, true)

				err := ioutil.WriteFile(cacheFilePath, fileContent, 0666)
				Ω(err).ShouldNot(HaveOccurred())

				downloadContent = "now you don't"

				etag := "second-round-etag"
				returnedHeader.Set("ETag", etag)

				server.AppendHandlers(ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/my_file"),
					http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
						Ω(req.Header.Get("If-None-Match")).Should(Equal("my-original-etag"))
					}),
					ghttp.RespondWithPtr(&status, &downloadContent, returnedHeader),
				))
			})

			It("should perform the request with the correct modified headers", func() {
				cache.Fetch(url, true)
				Ω(server.ReceivedRequests()).Should(HaveLen(2))
			})

			Context("if the file has been modified", func() {
				BeforeEach(func() {
					status = http.StatusOK
				})

				It("should redownload the file", func() {
					cache.Fetch(url, true)
					Ω(ioutil.ReadFile(cacheFilePath)).Should(Equal([]byte(downloadContent)))
				})

				It("should return a readcloser pointing to the file", func() {
					file, err := cache.Fetch(url, true)
					Ω(err).ShouldNot(HaveOccurred())
					Ω(ioutil.ReadAll(file)).Should(Equal([]byte(downloadContent)))
				})

				It("should have put the file in the cache", func() {
					_, err := cache.Fetch(url, true)
					Ω(err).ShouldNot(HaveOccurred())
					Ω(ioutil.ReadDir(cachedPath)).Should(HaveLen(1))
					Ω(ioutil.ReadDir(uncachedPath)).Should(HaveLen(0))
				})
			})

			Context("if the file has been modified, but the new file has no etag", func() {
				BeforeEach(func() {
					status = http.StatusOK
					returnedHeader.Del("ETag")
				})

				It("should return a readcloser pointing to the file", func() {
					file, err := cache.Fetch(url, true)
					Ω(err).ShouldNot(HaveOccurred())
					Ω(ioutil.ReadAll(file)).Should(Equal([]byte(downloadContent)))
				})

				It("should have removed the file from the cache", func() {
					_, err := cache.Fetch(url, true)
					Ω(err).ShouldNot(HaveOccurred())
					Ω(ioutil.ReadDir(cachedPath)).Should(HaveLen(0))
					Ω(ioutil.ReadDir(uncachedPath)).Should(HaveLen(0))
				})
			})

			Context("if the file has not been modified", func() {
				BeforeEach(func() {
					status = http.StatusNotModified
				})

				It("should not redownload the file", func() {
					_, err := cache.Fetch(url, true)
					Ω(err).ShouldNot(HaveOccurred())
					Ω(ioutil.ReadFile(cacheFilePath)).Should(Equal(fileContent))
				})

				It("should return a readcloser pointing to the file", func() {
					file, err := cache.Fetch(url, true)
					Ω(err).ShouldNot(HaveOccurred())
					Ω(ioutil.ReadAll(file)).Should(Equal(fileContent))
				})
			})
		})

		Context("when the file size exceeds the total available cache size", func() {
			BeforeEach(func() {
				downloadContent = []byte(strings.Repeat("7", int(maxSizeInBytes*3)))

				server.AppendHandlers(ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/my_file"),
					ghttp.RespondWith(http.StatusOK, string(downloadContent), returnedHeader),
				))

				file, err = cache.Fetch(url, true)
			})

			It("should not error", func() {
				Ω(err).ShouldNot(HaveOccurred())
			})

			It("should return a readCloser that streams the file", func() {
				Ω(file).ShouldNot(BeNil())
				Ω(ioutil.ReadAll(file)).Should(Equal(downloadContent))
			})

			It("should put the file in the uncached path, then delete it", func() {
				err := file.Close()
				Ω(err).ShouldNot(HaveOccurred())
				Ω(ioutil.ReadDir(cachedPath)).Should(HaveLen(0))
				Ω(ioutil.ReadDir(uncachedPath)).Should(HaveLen(0))
			})
		})

		Context("when the cache is full", func() {
			It("deletes the oldest cached files until there is space", func() {
				//read them one at a time
				downloadContent = []byte(strings.Repeat("C", int(maxSizeInBytes/4)))
				url, _ = Url.Parse(server.URL() + "/C")
				server.AppendHandlers(ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/C"),
					ghttp.RespondWith(http.StatusOK, string(downloadContent), returnedHeader),
				))

				cachedFile1, err := cache.Fetch(url, true)
				Ω(err).ShouldNot(HaveOccurred())
				cachedFile1.Close()

				downloadContent = []byte(strings.Repeat("A", int(maxSizeInBytes/4)))
				url, _ = Url.Parse(server.URL() + "/A")
				server.AppendHandlers(ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/A"),
					ghttp.RespondWith(http.StatusOK, string(downloadContent), returnedHeader),
				))

				cachedFile2, err := cache.Fetch(url, true)
				Ω(err).ShouldNot(HaveOccurred())
				cachedFile2.Close()

				downloadContent = []byte(strings.Repeat("B", int(maxSizeInBytes/4)))
				url, _ = Url.Parse(server.URL() + "/B")
				server.AppendHandlers(ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/B"),
					ghttp.RespondWith(http.StatusOK, string(downloadContent), returnedHeader),
				))

				cachedFile3, err := cache.Fetch(url, true)
				Ω(err).ShouldNot(HaveOccurred())
				cachedFile3.Close()

				url, _ = Url.Parse(server.URL() + "/C")
				server.AppendHandlers(ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/C"),
					ghttp.RespondWith(http.StatusNotModified, ""),
				))

				cachedFile1, err = cache.Fetch(url, true)
				Ω(err).ShouldNot(HaveOccurred())
				cachedFile1.Close()

				//try to add a file that has size 513
				downloadContent = []byte(strings.Repeat("D", int(maxSizeInBytes/2)+1))

				url, _ = Url.Parse(server.URL() + "/D")
				server.AppendHandlers(ghttp.CombineHandlers(
					ghttp.VerifyRequest("GET", "/D"),
					ghttp.RespondWith(http.StatusOK, string(downloadContent), returnedHeader),
				))

				cachedFile4, err := cache.Fetch(url, true)
				Ω(err).ShouldNot(HaveOccurred())
				Ω(ioutil.ReadAll(cachedFile4)).Should(Equal(downloadContent))

				//make sure we removed the two we read first
				Ω(ioutil.ReadDir(cachedPath)).Should(HaveLen(2))

				_, err = os.Stat(filepath.Join(cachedPath, computeMd5(server.URL()+"/A")))
				Ω(err).Should(HaveOccurred())
				_, err = os.Stat(filepath.Join(cachedPath, computeMd5(server.URL()+"/B")))
				Ω(err).Should(HaveOccurred())

				_, err = os.Stat(filepath.Join(cachedPath, computeMd5(server.URL()+"/C")))
				Ω(err).ShouldNot(HaveOccurred())
				_, err = os.Stat(filepath.Join(cachedPath, computeMd5(server.URL()+"/D")))
				Ω(err).ShouldNot(HaveOccurred())
			})
		})
	})

	Context("when the URL has query parameters", func() {
		It("ignores the query parameters for the purposes of caching", func() {
			returnedHeader := http.Header{}
			returnedHeader.Set("ETag", "my-original-etag")

			// download some stuff
			downloadContent = []byte("hi")
			url, _ = Url.Parse(server.URL() + "/A?param=1")
			server.AppendHandlers(ghttp.CombineHandlers(
				ghttp.VerifyRequest("GET", "/A", "param=1"),
				ghttp.RespondWith(http.StatusOK, string(downloadContent), returnedHeader),
			))
			_, err := cache.Fetch(url, true)
			Ω(err).ShouldNot(HaveOccurred())

			returnedHeader.Set("ETag", "my-new-etag")
			// download some different stuff from the same path with different query string
			downloadContent = []byte("hello")
			url, _ = Url.Parse(server.URL() + "/A?param=2")
			server.AppendHandlers(ghttp.CombineHandlers(
				ghttp.VerifyRequest("GET", "/A", "param=2"),
				ghttp.RespondWith(http.StatusOK, string(downloadContent), returnedHeader),
			))
			_, err = cache.Fetch(url, true)
			Ω(err).ShouldNot(HaveOccurred())

			Ω(ioutil.ReadDir(cachedPath)).Should(HaveLen(1))
			path := filepath.Join(cachedPath, computeMd5(server.URL()+"/A"))
			Ω(ioutil.ReadFile(path)).Should(Equal(downloadContent))
		})
	})
})
