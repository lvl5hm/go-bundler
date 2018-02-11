package main

import (
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lvl5hm/go-bundler/jsLoader"
)

type safeFile struct {
	file *os.File
	lock sync.RWMutex
}

func newSafeFile(fileName string) *safeFile {
	file, err := os.Create(fileName)
	if err != nil {
		panic(err)
	}

	return &safeFile{file, sync.RWMutex{}}
}

func (sf *safeFile) write(data []byte) {
	sf.lock.Lock()
	defer sf.lock.Unlock()

	sf.file.Write(data)
}

func (sf *safeFile) close() {
	sf.file.Close()
}

type fileCache struct {
	Data        []byte
	LastModTime time.Time
	Imports     []string
	IsReachable bool
}

type bundleCache struct {
	Files   map[string]fileCache
	DirName string
	Lock    sync.RWMutex
}

func (c *bundleCache) read(fileName string) (fileCache, bool) {
	c.Lock.RLock()
	defer c.Lock.RUnlock()

	file, ok := c.Files[fileName]
	return file, ok
}

func (c *bundleCache) write(fileName string, data fileCache) {
	c.Lock.Lock()
	defer c.Lock.Unlock()

	c.Files[fileName] = data
}

func (c *bundleCache) saveFile() {
	if c.DirName == "" {
		return
	}

	err := os.MkdirAll(c.DirName, 0666)
	if err != nil {
		fmt.Println(err)
		return
	}

	saveFile, err := os.Create(c.DirName + "/cache")
	if err != nil {
		fmt.Println(err)
		// fmt.Println("Error: cannot create save file for cache!")
		return
	}
	defer saveFile.Close()

	enc := gob.NewEncoder(saveFile)
	err = enc.Encode(c.Files)
	if err != nil {
		fmt.Println("Error: cannot save cache to file!")
	}
	// fmt.Printf(">>Cache saved to %s\n", c.SaveFileName)
}

func (c *bundleCache) loadFile() {
	saveFile, err := os.Open(c.DirName + "/cache")
	if err != nil {
		return
	}
	defer saveFile.Close()

	dec := gob.NewDecoder(saveFile)

	var files map[string]fileCache
	err = dec.Decode(&files)
	if err != nil {
		fmt.Println("Error: cache file is corrupted!")
		return
	}

	c.Files = files
}

type configJSON struct {
	Entry        string
	TemplateHTML string
	BundleDir    string
	WatchFiles   bool
	DevServer    struct {
		Enable bool
		Port   int
	}
	PermanentCache struct {
		Enable  bool
		DirName string
	}
}

func main() {
	// setting up config
	config := configJSON{}

	configFileName := "config.json"
	if len(os.Args) > 1 {
		configFileName = os.Args[1]
	}

	configFile, err := ioutil.ReadFile(configFileName)
	if err != nil {
		fmt.Println("Unable to load config file!")
	} else {
		json.Unmarshal(configFile, &config)
	}

	// config defaults
	if config.Entry == "" {
		config.Entry = "index.js"
	}
	if config.BundleDir == "" {
		config.BundleDir = "build"
	}
	if config.DevServer.Port == 0 {
		config.DevServer.Port = 8080
	}
	if config.PermanentCache.DirName == "" {
		config.PermanentCache.DirName = ".go-bundler-cache"
	}

	// creating bundle
	bundleName := filepath.Join(config.BundleDir, "bundle.js")

	cache := &bundleCache{}
	if config.PermanentCache.Enable {
		cache.DirName = config.PermanentCache.DirName
	}

	cache.loadFile()
	if cache.Files == nil {
		cache.Files = map[string]fileCache{}
	}

	createBundle(config.Entry, bundleName, cache)

	if config.TemplateHTML != "" {
		bundleHTMLTemplate(config.TemplateHTML, bundleName)
	}

	// dev server and watching files
	if config.DevServer.Enable {
		if config.WatchFiles {
			go watchBundledFiles(cache, config.Entry, bundleName)
		}
		fmt.Printf("Dev server listening at port %v\n", config.DevServer.Port)
		server := http.FileServer(http.Dir(config.BundleDir))
		err := http.ListenAndServe(fmt.Sprintf(":%v", config.DevServer.Port), server)
		log.Fatal(err)
	} else if config.WatchFiles {
		watchBundledFiles(cache, config.Entry, bundleName)
	}
}

func indexOf(arr []string, str string) int {
	for i, item := range arr {
		if item == str {
			return i
		}
	}
	return -1
}

func createBundle(entryFileName, bundleFileName string, cache *bundleCache) {
	buildStartTime := time.Now()

	os.MkdirAll(filepath.Dir(bundleFileName), 0666)
	os.Remove(bundleFileName)
	sf := newSafeFile(bundleFileName)
	defer sf.close()

	// mark all files as unreachable at the start of the build
	// so the autorebuilder does not try to rebuild when they change
	for fileName, file := range cache.Files {
		file.IsReachable = false
		cache.Files[fileName] = file
	}

	sf.write([]byte("var moduleFns={},modules={};var process={env:{NODE_ENV:'development'}};"))
	err := addFilesToBundle([]string{entryFileName}, sf, cache)
	sf.write(getJsBundleFileTail(entryFileName, cache))

	if err == nil {
		fmt.Printf("\n>>Build finished in %s\n", time.Since(buildStartTime))
	} else {
		fmt.Printf("\n>>%s\n", err)
	}

	cache.saveFile()
}

func getJsBundleFileTail(entryFileName string, cache *bundleCache) []byte {
	moduleOrder := []string{}

	var createImportTree func(string, []string)
	createImportTree = func(fileName string, path []string) {
		if filepath.Ext(fileName) != ".js" {
			return
		}

		if i := indexOf(path, fileName); i >= 0 {
			fmt.Printf(
				"\n>>Warning: circular dependency detected:\n%s\n",
				strings.Join(append(path[i:], fileName), " -> "),
			)
			return
		}

		file := cache.Files[fileName]
		for _, importPath := range file.Imports {
			createImportTree(importPath, append(path, fileName))
		}

		moduleName := "'" + jsLoader.CreateVarNameFromPath(fileName) + "'"
		if indexOf(moduleOrder, moduleName) < 0 {
			moduleOrder = append(moduleOrder, moduleName)
		}
	}

	createImportTree(entryFileName, []string{})
	jsModuleOrder := fmt.Sprintf("var moduleOrder = [%s];", strings.Join(moduleOrder, ","))
	result := []byte(jsModuleOrder + "moduleOrder.forEach((moduleName)=>modules[moduleName]=moduleFns[moduleName]())")

	return result
}

func addFilesToBundle(
	files []string,
	bundleSf *safeFile,
	cache *bundleCache,
) error {
	errorCh := make(chan error, len(files))

	for _, unbundledFile := range files {
		addFileToBundle(unbundledFile, bundleSf, errorCh, cache)
	}

	for counter := 0; counter < len(files); counter++ {
		err := <-errorCh
		if err != nil {
			return err
		}
	}

	return nil
}

type fileError struct {
	err  string
	path string
}

func (fe fileError) Error() string {
	return "Error: " + fe.err + " " + fe.path
}

func addFileToBundle(
	resolvedPath string,
	bundleSf *safeFile,
	errorCh chan error,
	cache *bundleCache,
) {
	var data []byte
	var fileImports []string

	fileStats, err := os.Stat(resolvedPath)
	if err != nil {
		errorCh <- fileError{">>Error: cannot find file", resolvedPath}
		return
	}
	lastModTime := fileStats.ModTime()

	saveCache := func() {
		cache.write(resolvedPath, fileCache{
			Data:        data,
			Imports:     fileImports,
			LastModTime: lastModTime,
			IsReachable: true,
		})
	}

	cache.Lock.Lock()
	cachedFile, ok := cache.Files[resolvedPath]
	if ok && cachedFile.IsReachable {
		cache.Lock.Unlock()
		errorCh <- nil
		return
	}
	cache.Files[resolvedPath] = fileCache{
		IsReachable: true,
	}
	cache.Lock.Unlock()

	if ok && cachedFile.LastModTime == fileStats.ModTime() {
		data = cachedFile.Data
		fileImports = cachedFile.Imports
	} else {
		ext := filepath.Ext(resolvedPath)

		//fmt.Printf("Loading %s\n", resolvedPath)
		switch ext {
		case ".js":
			src, err := ioutil.ReadFile(resolvedPath)
			if err != nil {
				saveCache()
				errorCh <- err
				return
			}

			data, fileImports, err = jsLoader.LoadFile(src, resolvedPath)
			if err != nil {
				saveCache()
				errorCh <- err
				return
			}

		default:
			bundleDir := filepath.Dir(bundleSf.file.Name())
			dstFileName := bundleDir + "/" + jsLoader.CreateVarNameFromPath(resolvedPath) + ext
			copyFile(dstFileName, resolvedPath)
		}
	}

	bundleSf.write(data)

	saveCache()
	err = addFilesToBundle(fileImports, bundleSf, cache)

	errorCh <- err
}

func bundleHTMLTemplate(templateName, bundleName string) {
	template, err := ioutil.ReadFile(templateName)
	if err != nil {
		log.Fatal("Can't find or open html template")
	}

	templateStr := string(template)
	insertIndex := strings.Index(templateStr, "\n</body")
	if insertIndex < 0 {
		log.Fatal("Can't find end of <body> in html template")
	}

	result := templateStr[:insertIndex] +
		"\n  <script src=\"" + filepath.Base(bundleName) + "\"></script>\n" +
		templateStr[insertIndex+1:]

	bundleDir := filepath.Dir(bundleName)
	ioutil.WriteFile(filepath.Join(bundleDir, "index.html"), []byte(result), 0666)
}

func copyFile(dst, src string) {
	from, err := os.Open(src)
	if err != nil {
		fmt.Println(err)
	}
	defer from.Close()

	to, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		fmt.Println(err)
	}
	defer to.Close()
	io.Copy(to, from)
}

func watchBundledFiles(
	cache *bundleCache,
	entryName,
	bundleName string,
) func() {
	fmt.Println("Watching for file changes")

	running := true

	checkFiles := func() {
		for running {
			for path, file := range cache.Files {
				if !file.IsReachable {
					continue
				}

				stats, err := os.Stat(path)
				if err != nil {
					continue
				}
				if file.LastModTime != stats.ModTime() {
					createBundle(entryName, bundleName, cache)
					break
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	checkFiles()

	return func() {
		fmt.Println("Stopped watching files")
		running = false
	}
}
