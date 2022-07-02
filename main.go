package main

import (
	"bytes"
	config2 "chunk-executor/config"
	"chunk-executor/models"
	"encoding/json"
	"fmt"
	"github.com/BurntSushi/toml"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"io"
	"io/ioutil"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type ServerInstance struct {
	Name         string
	ChunkCh      chan *models.Chunk
	FreeChunks   int
	FreeChunksMx sync.Mutex
	SSHClient    *ssh.Client
	STFPClient   *sftp.Client
	CurrentTask  int
}

func (s *ServerInstance) ExecuteCmd(command string) (string, error) {
	var stdoutBuf bytes.Buffer

	session, err := s.SSHClient.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()

	session.Stdout = &stdoutBuf
	err = session.Run(command)
	if err != nil {
		return "", err
	}

	return stdoutBuf.String(), nil
}

func (s *ServerInstance) GetLoadAverage() (string, error) {
	return s.ExecuteCmd("uptime")
}

func main() {
	var config config2.Config
	if _, err := toml.DecodeFile("./config/config.toml", &config); err != nil {
		fmt.Println(err)
		return
	}

	files, err := ioutil.ReadDir("./logs")
	if err != nil {
		log.Fatal(err)
	}

	var maxTaskID int
	for _, file := range files {
		taskID, err := strconv.Atoi(file.Name()[:len(file.Name())-4])
		if err != nil {
			log.Fatal(err)
		}

		if taskID > maxTaskID {
			maxTaskID = taskID
		}
	}

	err = os.Mkdir("outputs/"+strconv.Itoa(maxTaskID+1), os.ModePerm)
	if err != nil {
		log.Fatal(err)
	}

	logFile, err := os.Create("./logs/" + strconv.Itoa(maxTaskID+1) + ".txt")
	if err != nil {
		log.Panic(err.Error())
	}

	var workDoneCounter int32

	servers := make([]*ServerInstance, 0, len(config.Servers))

	var wg sync.WaitGroup
	var serverMutex sync.Mutex

	wg.Add(len(config.Servers))
	for serverName, serverCfg := range config.Servers {
		go func(servName string, servCfg *config2.Server) {
			defer wg.Done()

			conn, err := ssh.Dial("tcp", servCfg.IP+":22", &ssh.ClientConfig{
				User: servCfg.User,
				Auth: []ssh.AuthMethod{
					ssh.Password(servCfg.Password),
				},
				HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			})
			if err != nil {
				logFile.Write([]byte(err.Error()))
				log.Panic("server " + servName + "panic: " + err.Error())
			}

			sc, err := sftp.NewClient(conn)
			if err != nil {
				log.Fatalf("Unable to start SFTP subsystem: %v", err)
			}

			serverInstance := &ServerInstance{
				Name:         servName,
				ChunkCh:      make(chan *models.Chunk),
				SSHClient:    conn,
				STFPClient:   sc,
				FreeChunks:   servCfg.MaxChunks,
				FreeChunksMx: sync.Mutex{},
				CurrentTask:  0,
			}

			//// getting all tasks directories
			//output, err := serverInstance.ExecuteCmd("find . -maxdepth 1 -type d -regex \".*[0-9]\"")
			//if err != nil {
			//	log.Panic("server " + servName + "panic: " + err.Error())
			//}
			//taskDirs := strings.Split(output, "\n")
			//var maxTaskID int
			//for _, taskDir := range taskDirs {
			//	if len(taskDir) != 0 {
			//		currentTaskID, err := strconv.Atoi(taskDir[2:])
			//		if err != nil {
			//			if err != nil {
			//				log.Panic("server " + servName + "panic: " + err.Error())
			//			}
			//		}
			//		if currentTaskID > maxTaskID {
			//			maxTaskID = currentTaskID
			//		}
			//	}
			//}

			serverInstance.CurrentTask = maxTaskID + 1

			go serverInstance.startHandlingChunks(&config, logFile, &workDoneCounter)

			serverMutex.Lock()
			servers = append(servers, serverInstance)
			serverMutex.Unlock()
		}(serverName, serverCfg)
	}

	wg.Wait()

	file, err := os.Open("chunk/chunks.json")
	if err != nil {
		log.Panic(err.Error())
	}

	byteValue, err := ioutil.ReadAll(file)
	if err != nil {
		log.Panic(err.Error())
	}

	var chunksBase models.ChunksBase

	err = json.Unmarshal(byteValue, &chunksBase)
	if err != nil {
		log.Panic(err.Error())
	}

	var currentServerIndex int

	for _, chunk := range chunksBase.Chunks {
		currentServerIndex = currentServerIndex % len(servers)
		serverInstance := servers[currentServerIndex]
		serverInstance.FreeChunksMx.Lock()
		if serverInstance.FreeChunks > 0 {
			serverInstance.ChunkCh <- chunk
			serverInstance.FreeChunks--
		}
		serverInstance.FreeChunksMx.Unlock()
		currentServerIndex++
	}

	workDoneCh := make(chan struct{})

	go waitUntilWorkDone(&workDoneCounter, chunksBase.Chunks, workDoneCh)

	<-workDoneCh

	fmt.Println("work done!")
}

func waitUntilWorkDone(workDoneCounter *int32, chunks []*models.Chunk, workDoneCh chan<- struct{}) {
	for int(*workDoneCounter) != len(chunks) {
		continue
	}

	workDoneCh <- struct{}{}
}

func (s *ServerInstance) startHandlingChunks(config *config2.Config, logFile *os.File, workDoneCounter *int32) {
	for i := 0; i < s.FreeChunks; i++ {
		go func() {
			for {
				chunk := <-s.ChunkCh

				_, err := s.ExecuteCmd("mkdir -p " + strconv.Itoa(s.CurrentTask) + "/" + strconv.Itoa(chunk.ChunkID))
				if err != nil {
					log.Panic(err.Error())
				}

				chunkFileNameSplits := strings.Split(chunk.File, "/")
				chunkFileName := chunkFileNameSplits[len(chunkFileNameSplits)-1]

				catFileLocal, err := os.Open(chunk.File)
				if err != nil {
					LA, LaErr := s.GetLoadAverage()
					if LaErr != nil {
						LA = LaErr.Error()
					}

					_, err = logFile.Write([]byte(generateLogOutput(chunk.ChunkID, time.Now(), err.Error(), LA)))
					if err != nil {
						log.Panic(err.Error())
					}

					atomic.AddInt32(workDoneCounter, 1)
					continue
				}

				outputFileName := strconv.Itoa(chunk.ChunkID) + ".log"
				outputDir := strconv.Itoa(s.CurrentTask)
				outputFileNameArchived := fmt.Sprintf("%v.log.tar.gz", chunk.ChunkID)

				catFileRemote, err := s.STFPClient.Create(outputDir + "/" + chunkFileName)
				if err != nil {
					LA, LaErr := s.GetLoadAverage()
					if LaErr != nil {
						LA = LaErr.Error()
					}

					_, err = logFile.Write([]byte(generateLogOutput(chunk.ChunkID, time.Now(), err.Error(), LA)))
					if err != nil {
						log.Panic(err.Error())
					}

					atomic.AddInt32(workDoneCounter, 1)
					catFileLocal.Close()
					continue
				}

				_, err = io.Copy(catFileRemote, catFileLocal)
				if err != nil {
					LA, LaErr := s.GetLoadAverage()
					if LaErr != nil {
						LA = LaErr.Error()
					}

					_, err = logFile.Write([]byte(generateLogOutput(chunk.ChunkID, time.Now(), err.Error(), LA)))
					if err != nil {
						log.Panic(err.Error())
					}

					atomic.AddInt32(workDoneCounter, 1)
					catFileLocal.Close()
					catFileRemote.Close()
					continue
				}

				params := make(map[string]string)
				err = json.Unmarshal(chunk.Params, &params)
				if err != nil {
					log.Panic(err.Error())
				}

				paramsOrder := strings.Split(config.Binary.ParamsOrder, ",")
				values := make([]interface{}, 0, len(paramsOrder))
				for _, param := range paramsOrder {
					values = append(values, params[param])
				}

				paramsArgsFilled := fmt.Sprintf(config.Binary.Params, values...)

				command := fmt.Sprintf("cat '%v' | %v %v > %v/%v.log", outputDir+"/"+chunkFileName, config.Binary.BinaryPath, paramsArgsFilled, s.CurrentTask, chunk.ChunkID)

				_, err = s.ExecuteCmd(command)
				if err != nil {
					LA, LaErr := s.GetLoadAverage()
					if LaErr != nil {
						LA = LaErr.Error()
					}

					_, err = logFile.Write([]byte(generateLogOutput(chunk.ChunkID, time.Now(), err.Error(), LA)))
					if err != nil {
						log.Panic(err.Error())
					}

					atomic.AddInt32(workDoneCounter, 1)
					catFileLocal.Close()
					catFileRemote.Close()
					continue
				}

				_, err = s.ExecuteCmd("cd " + outputDir + " && tar -zcvf " + outputFileNameArchived + " " + outputFileName + " && cd -")
				if err != nil {
					LA, LaErr := s.GetLoadAverage()
					if LaErr != nil {
						LA = LaErr.Error()
					}

					_, err = logFile.Write([]byte(generateLogOutput(chunk.ChunkID, time.Now(), err.Error(), LA)))
					if err != nil {
						log.Panic(err.Error())
					}

					atomic.AddInt32(workDoneCounter, 1)
					catFileLocal.Close()
					catFileRemote.Close()
					continue
				}

				srcFile, err := s.STFPClient.OpenFile(outputDir+"/"+outputFileNameArchived, os.O_RDONLY)
				if err != nil {
					LA, LaErr := s.GetLoadAverage()
					if LaErr != nil {
						LA = LaErr.Error()
					}

					_, err = logFile.Write([]byte(generateLogOutput(chunk.ChunkID, time.Now(), err.Error(), LA)))
					if err != nil {
						log.Panic(err.Error())
					}

					atomic.AddInt32(workDoneCounter, 1)
					catFileLocal.Close()
					catFileRemote.Close()
					continue
				}

				dstFile, err := os.Create("outputs/" + outputDir + "/" + outputFileNameArchived)
				if err != nil {
					LA, LaErr := s.GetLoadAverage()
					if LaErr != nil {
						LA = LaErr.Error()
					}

					_, err = logFile.Write([]byte(generateLogOutput(chunk.ChunkID, time.Now(), err.Error(), LA)))
					if err != nil {
						log.Panic(err.Error())
					}

					atomic.AddInt32(workDoneCounter, 1)
					srcFile.Close()
					catFileLocal.Close()
					catFileRemote.Close()
					continue
				}

				_, err = io.Copy(dstFile, srcFile)
				if err != nil {
					LA, LaErr := s.GetLoadAverage()
					if LaErr != nil {
						LA = LaErr.Error()
					}

					_, err = logFile.Write([]byte(generateLogOutput(chunk.ChunkID, time.Now(), err.Error(), LA)))
					if err != nil {
						log.Panic(err.Error())
					}

					atomic.AddInt32(workDoneCounter, 1)
					dstFile.Close()
					srcFile.Close()
					catFileLocal.Close()
					catFileRemote.Close()
					continue
				}

				_, err = s.ExecuteCmd("rm -rf " + outputDir)
				if err != nil {
					LA, LaErr := s.GetLoadAverage()
					if LaErr != nil {
						LA = LaErr.Error()
					}

					_, err = logFile.Write([]byte(generateLogOutput(chunk.ChunkID, time.Now(), err.Error(), LA)))
					if err != nil {
						log.Panic(err.Error())
					}

					atomic.AddInt32(workDoneCounter, 1)
					dstFile.Close()
					srcFile.Close()
					catFileLocal.Close()
					catFileRemote.Close()
					continue
				}

				LA, err := s.GetLoadAverage()
				if err != nil {
					LA = err.Error()
				}

				_, err = logFile.Write([]byte(generateLogOutput(chunk.ChunkID, time.Now(), "SUCCESS", LA)))
				if err != nil {
					dstFile.Close()
					srcFile.Close()
					catFileLocal.Close()
					catFileRemote.Close()
					log.Panic(err.Error())
				}

				s.FreeChunksMx.Lock()
				s.FreeChunks++
				s.FreeChunksMx.Unlock()
				atomic.AddInt32(workDoneCounter, 1)
				srcFile.Close()
				dstFile.Close()
			}
		}()
	}
}

func generateLogOutput(ChunkID int, Time time.Time, Message string, LoadAverage string) string {
	return fmt.Sprintf("ChunkID: %v, Time: %v, Status: %v, LA: %v", ChunkID, Time.String(), Message, LoadAverage)
}
