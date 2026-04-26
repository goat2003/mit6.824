package main

import (
	"fmt"
	"sync"
)

// Fetcher hides the real page fetch operation. The examples below use a fake
// implementation so the crawler can run deterministically without network I/O.
type Fetcher interface {
	Fetch(url string) (body string, urls []string, err error)
}

// 串行爬取url
func Serial(url string, fetcher Fetcher, fetched map[string]bool) {
	if fetched[url] {
		return
	}
	fetched[url] = true

	body, urls, err := fetcher.Fetch(url)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("found: %s %q\n", url, body)

	for _, next := range urls {
		Serial(next, fetcher, fetched)
	}
}

// 用mutex锁保护共享map的并行爬取
func ConcurrentMutex(url string, fetcher Fetcher, fetched map[string]bool, mu *sync.Mutex, wg *sync.WaitGroup) {
	defer wg.Done()   //表示执行完了一个任务，等待任务数-1

	mu.Lock()
	if fetched[url] {
		mu.Unlock()
		return
	}
	fetched[url] = true
	mu.Unlock()

	body, urls, err := fetcher.Fetch(url)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("found: %s %q\n", url, body)

	for _, next := range urls {
		wg.Add(1)    //表示新增了一个任务，等待任务数+1
		go ConcurrentMutex(next, fetcher, fetched, mu, wg)
	}
}

// 使用channel管理状态
func ConcurrentChannel(root string, fetcher Fetcher) {
	type result struct {
		urls []string
	}
	//work负责接收urls
	work := make(chan []string)
	done := make(chan result)

	go func() {
		work <- []string{root}
	}()

	fetched := make(map[string]bool)
	//表示当前没完成的任务数量
	outstanding := 1

	for outstanding > 0 {
		select {
		case urls := <-work:
			for _, url := range urls {
				if fetched[url] {
					continue
				}
				fetched[url] = true
				//发现了一个新的资源网站，任务数++
				outstanding++
				//提取这个新发现的资源的urls，并将urls发送给 done 通道
				go func(url string) {
					body, urls, err := fetcher.Fetch(url)
					if err != nil {
						fmt.Println(err)
						//哪怕提取urls出错了，也要将这个空urls(result{})送到done通道，表示这个任务已完成
						done <- result{}
						return
					}
					fmt.Printf("found: %s %q\n", url, body)
					done <- result{urls: urls}
				}(url)   //这个代码非常重要，如果不是捕获变量而是直接引用，就会使循环变量存储到堆空间导致引用的循环变量随着循环不断改变
			}
			//将整个urls遍历完，任务结束
			outstanding--


		//为什么一定要用done通道处理任务结束，如果不适用done来处理，那么就需要在 case work的for循环产生的goroutine函数中进行任务结束处理
		//也就是对oustanding进行--操作，可这样就会造成数据竞争
		//注意这个模型的本质就是只是用一个goroutine对任务数量进行操作，从而不使用mutex来避免数据竞争

		//那么在fetched[url] = true 时，不进行outstanding++操作，岂不是就不用进行任务结束处理了吗？
		//错误的，如果这样的话，可能会导致goroutine还在进行urls提取时，for循环就已经执行完毕进而触发outstanding--，从而导致主goroutine误以为没有任务了
		case r := <-done:
			//done通道负责对任务数进行操作，每收到一个urls就代表完成了一个任务
			outstanding--
			//继续将urls派送给work通道进行处理
			if len(r.urls) > 0 {
				outstanding++
				go func(urls []string) {
					work <- urls
				}(r.urls)
			}
		}
	}
}



func main() {
	fmt.Println("=== Serial ===")
	Serial("https://golang.org/", fetcher, make(map[string]bool))

	fmt.Println("\n=== Concurrent with mutex ===")
	var mu sync.Mutex
	var wg sync.WaitGroup
	fetched := make(map[string]bool)
	wg.Add(1)
	go ConcurrentMutex("https://golang.org/", fetcher, fetched, &mu, &wg)
	wg.Wait()

	fmt.Println("\n=== Concurrent with channels ===")
	ConcurrentChannel("https://golang.org/", fetcher)
}


// 模拟一个爬虫工具爬取出来的所有资源
type fakeFetcher map[string]*fakeResult

// 模拟爬取出来的单个资源
type fakeResult struct {
	body string
	urls []string
}

func (f fakeFetcher) Fetch(url string) (string, []string, error) {
	//先寻找单个资源是否存在
	if res, ok := f[url]; ok {
	//如果存在的话返回这个资源
		return res.body, res.urls, nil
	}
	return "", nil, fmt.Errorf("not found: %s", url)
}

var fetcher = fakeFetcher{
	"https://golang.org/": {
		"The Go Programming Language",
		[]string{
			"https://golang.org/pkg/",
			"https://golang.org/cmd/",
		},
	},
	"https://golang.org/pkg/": {
		"Packages",
		[]string{
			"https://golang.org/",
			"https://golang.org/cmd/",
			"https://golang.org/pkg/fmt/",
			"https://golang.org/pkg/os/",
		},
	},
	"https://golang.org/pkg/fmt/": {
		"Package fmt",
		[]string{
			"https://golang.org/",
			"https://golang.org/pkg/",
		},
	},
	"https://golang.org/pkg/os/": {
		"Package os",
		[]string{
			"https://golang.org/",
			"https://golang.org/pkg/",
		},
	},
}
