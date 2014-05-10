package builder_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/cloudfoundry-incubator/garden/client/fake_warden_client"
	"github.com/cloudfoundry-incubator/garden/warden"
	"github.com/gorilla/websocket"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/ghttp"

	"github.com/winston-ci/prole/api/builds"
	. "github.com/winston-ci/prole/builder"
	"github.com/winston-ci/prole/sourcefetcher/fakesourcefetcher"
)

var _ = Describe("Builder", func() {
	var sourceFetcher *fakesourcefetcher.Fetcher
	var wardenClient *fake_warden_client.FakeClient
	var builder Builder

	var build builds.Build

	primedStream := func(payloads ...warden.ProcessStream) <-chan warden.ProcessStream {
		stream := make(chan warden.ProcessStream, len(payloads))

		for _, payload := range payloads {
			stream <- payload
		}

		close(stream)

		return stream
	}

	BeforeEach(func() {
		sourceFetcher = fakesourcefetcher.New()
		wardenClient = fake_warden_client.New()

		builder = NewBuilder(sourceFetcher, wardenClient)

		build = builds.Build{
			Image: "some-image-name",

			Env: [][2]string{
				{"FOO", "bar"},
				{"BAZ", "buzz"},
			},
			Script: "./bin/test",

			Source: builds.BuildSource{
				Type: "raw",
				URI:  "http://example.com/foo.tar.gz",
				Path: "some/source/path",
			},
		}

		exitStatus := uint32(0)

		successfulStream := primedStream(warden.ProcessStream{
			ExitStatus: &exitStatus,
		})

		wardenClient.Connection.WhenRunning = func(handle string, spec warden.ProcessSpec) (uint32, <-chan warden.ProcessStream, error) {
			return 42, successfulStream, nil
		}

		wardenClient.Connection.WhenCreating = func(warden.ContainerSpec) (string, error) {
			return "some-handle", nil
		}

		tmpdir, err := ioutil.TempDir("", "stream-in-dir")
		Ω(err).ShouldNot(HaveOccurred())

		err = ioutil.WriteFile(filepath.Join(tmpdir, "some-file"), []byte("some-data"), 0644)
		Ω(err).ShouldNot(HaveOccurred())

		sourceFetcher.FetchResult = tmpdir
	})

	AfterEach(func() {
		os.RemoveAll(sourceFetcher.FetchResult)
	})

	It("creates a container with the specified image", func() {
		_, err := builder.Build(build)
		Ω(err).ShouldNot(HaveOccurred())

		created := wardenClient.Connection.Created()
		Ω(created).Should(HaveLen(1))
		Ω(created[0].RootFSPath).Should(Equal("image:some-image-name"))
	})

	It("fetches the build source and streams it in to the container", func() {
		_, err := builder.Build(build)
		Ω(err).ShouldNot(HaveOccurred())

		Ω(sourceFetcher.Fetched()).Should(ContainElement(build.Source))

		streamed := wardenClient.Connection.StreamedIn("some-handle")
		Ω(streamed).ShouldNot(BeEmpty())

		Ω(streamed[0].Destination).Should(Equal("some/source/path"))

		tarReader := tar.NewReader(bytes.NewBuffer(streamed[0].WriteBuffer.Contents()))

		hdr, err := tarReader.Next()
		Ω(err).ShouldNot(HaveOccurred())
		Ω(hdr.Name).Should(Equal("./"))

		hdr, err = tarReader.Next()
		Ω(err).ShouldNot(HaveOccurred())
		Ω(hdr.Name).Should(Equal("some-file"))
	})

	It("runs the build's script in the container", func() {
		_, err := builder.Build(build)
		Ω(err).ShouldNot(HaveOccurred())

		Ω(wardenClient.Connection.SpawnedProcesses("some-handle")).Should(ContainElement(warden.ProcessSpec{
			Script: "./bin/test",
			EnvironmentVariables: []warden.EnvironmentVariable{
				{"FOO", "bar"},
				{"BAZ", "buzz"},
			},
		}))
	})

	Context("when a config path is specified", func() {
		BeforeEach(func() {
			buildWithConfigPath := build
			buildWithConfigPath.ConfigPath = "config/path.yml"

			build = buildWithConfigPath
		})

		Context("and the path exists in the fetched source", func() {
			var contents string

			BeforeEach(func() {
				contents = ""
			})

			JustBeforeEach(func() {
				err := os.Mkdir(filepath.Join(sourceFetcher.FetchResult, "config"), 0755)
				Ω(err).ShouldNot(HaveOccurred())

				err = ioutil.WriteFile(
					filepath.Join(sourceFetcher.FetchResult, "config", "path.yml"),
					[]byte(contents),
					0644,
				)
				Ω(err).ShouldNot(HaveOccurred())
			})

			Context("and the content is valid YAML", func() {
				BeforeEach(func() {
					contents = `---
image: some-reconfigured-image

path: some-reconfigured-path

script: some-reconfigured-script

env:
  - FOO=1
  - BAR=2
  - BAZ=3
`
				})

				It("loads it and uses it to reconfigure the build", func() {
					_, err := builder.Build(build)
					Ω(err).ShouldNot(HaveOccurred())

					created := wardenClient.Connection.Created()
					Ω(created).Should(HaveLen(1))
					Ω(created[0].RootFSPath).Should(Equal("image:some-reconfigured-image"))

					streamedIn := wardenClient.Connection.StreamedIn("some-handle")
					Ω(streamedIn).Should(HaveLen(1))
					Ω(streamedIn[0].Destination).Should(Equal("some-reconfigured-path"))

					Ω(wardenClient.Connection.SpawnedProcesses("some-handle")).Should(ContainElement(warden.ProcessSpec{
						Script: "some-reconfigured-script",
						EnvironmentVariables: []warden.EnvironmentVariable{
							{"FOO", "1"},
							{"BAR", "2"},
							{"BAZ", "3"},
						},
					}))
				})

				Context("but the env is malformed", func() {
					BeforeEach(func() {
						contents = `---
image: some-reconfigured-image

script: some-reconfigured-script

env:
  - FOO
`
					})

					It("returns an error", func() {
						_, err := builder.Build(build)
						Ω(err).Should(HaveOccurred())
					})
				})
			})

			Context("and the contents are invalid", func() {
				BeforeEach(func() {
					contents = `ß`
				})

				It("returns an error", func() {
					_, err := builder.Build(build)
					Ω(err).Should(HaveOccurred())
				})
			})
		})

		Context("and the path does not exist", func() {
			It("returns an error", func() {
				_, err := builder.Build(build)
				Ω(err).Should(HaveOccurred())
			})
		})
	})

	Context("when a logs url is configured", func() {
		It("emits the build's output via websockets", func() {
			wardenClient.Connection.WhenRunning = func(handle string, spec warden.ProcessSpec) (uint32, <-chan warden.ProcessStream, error) {
				exitStatus := uint32(0)

				successfulStream := primedStream(
					warden.ProcessStream{
						Source: warden.ProcessStreamSourceStdout,
						Data:   []byte("stdout\n"),
					},
					warden.ProcessStream{
						Source: warden.ProcessStreamSourceStderr,
						Data:   []byte("stderr\n"),
					},
					warden.ProcessStream{
						ExitStatus: &exitStatus,
					},
				)

				return 42, successfulStream, nil
			}

			websocketEndpoint := ghttp.NewServer()

			buf := gbytes.NewBuffer()

			var upgrader = websocket.Upgrader{
				ReadBufferSize:  1024,
				WriteBufferSize: 1024,
				CheckOrigin: func(r *http.Request) bool {
					// allow all connections
					return true
				},
			}

			websocketEndpoint.AppendHandlers(
				func(w http.ResponseWriter, r *http.Request) {
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						log.Println(err)
						return
					}

					for {
						_, msg, err := conn.ReadMessage()
						if err != nil {
							break
						}

						buf.Write(msg)
					}
				},
			)

			build.LogsURL = "ws://" + websocketEndpoint.HTTPTestServer.Listener.Addr().String()

			_, err := builder.Build(build)
			Ω(err).ShouldNot(HaveOccurred())

			Eventually(buf).Should(gbytes.Say("creating container from some-image-name...\n"))
			Eventually(buf).Should(gbytes.Say("starting...\n"))
			Eventually(buf).Should(gbytes.Say("stdout\n"))
			Eventually(buf).Should(gbytes.Say("stderr\n"))
		})
	})

	Context("when running the build's script fails", func() {
		disaster := errors.New("oh no!")

		BeforeEach(func() {
			wardenClient.Connection.WhenRunning = func(handle string, spec warden.ProcessSpec) (uint32, <-chan warden.ProcessStream, error) {
				return 0, nil, disaster
			}
		})

		It("returns true", func() {
			succeeded, err := builder.Build(build)
			Ω(err).Should(Equal(disaster))
			Ω(succeeded).Should(BeFalse())
		})
	})

	Context("when the build's script exits 0", func() {
		BeforeEach(func() {
			wardenClient.Connection.WhenRunning = func(handle string, spec warden.ProcessSpec) (uint32, <-chan warden.ProcessStream, error) {
				exitStatus := uint32(0)

				return 42, primedStream(warden.ProcessStream{
					ExitStatus: &exitStatus,
				}), nil
			}
		})

		It("returns true", func() {
			succeeded, err := builder.Build(build)
			Ω(err).ShouldNot(HaveOccurred())

			Ω(succeeded).Should(BeTrue())
		})
	})

	Context("when the build's script exits nonzero", func() {
		BeforeEach(func() {
			wardenClient.Connection.WhenRunning = func(handle string, spec warden.ProcessSpec) (uint32, <-chan warden.ProcessStream, error) {
				exitStatus := uint32(2)

				return 42, primedStream(warden.ProcessStream{
					ExitStatus: &exitStatus,
				}), nil
			}
		})

		It("returns true", func() {
			succeeded, err := builder.Build(build)
			Ω(err).ShouldNot(HaveOccurred())
			Ω(succeeded).Should(BeFalse())
		})
	})

	Context("when creating the container fails", func() {
		disaster := errors.New("oh no!")

		BeforeEach(func() {
			wardenClient.Connection.WhenCreating = func(spec warden.ContainerSpec) (string, error) {
				return "", disaster
			}
		})

		It("returns the error", func() {
			succeeded, err := builder.Build(build)
			Ω(err).Should(Equal(disaster))
			Ω(succeeded).Should(BeFalse())
		})
	})

	Context("when fetching the source fails", func() {
		disaster := errors.New("oh no!")

		BeforeEach(func() {
			sourceFetcher.FetchError = disaster
		})

		It("returns the error", func() {
			succeeded, err := builder.Build(build)
			Ω(err).Should(Equal(disaster))
			Ω(succeeded).Should(BeFalse())
		})
	})

	Context("when copying the source in to the container fails", func() {
		disaster := errors.New("oh no!")

		BeforeEach(func() {
			wardenClient.Connection.WhenStreamingIn = func(handle string, dst string) (io.WriteCloser, error) {
				return nil, disaster
			}
		})

		It("returns the error", func() {
			succeeded, err := builder.Build(build)
			Ω(err).Should(Equal(disaster))
			Ω(succeeded).Should(BeFalse())
		})
	})
})