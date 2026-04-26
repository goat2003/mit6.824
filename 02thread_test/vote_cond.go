package main

import (
	"fmt"
	"math/rand"
	"sync"
	"time"
)


func main() {
	rand.Seed(time.Now().UnixNano())
	
	count :=0
	finished :=0
	var mu sync.Mutex
	cond := sync.NewCond(&mu)

	for i:=0;i<10;i++{
		go func() {
			vote := Vote()
			mu.Lock()
			defer mu.Unlock()
			defer cond.Broadcast()
			if vote{
				count++
			}
			finished++
			cond.Broadcast()
		}()
	}

	for count<5 && finished!=10{    //增加条件变量，等待其他线程结果，减少cpu的浪费
		cond.Wait()   
	}
	if count>=5{
		fmt.Println("success!")
	}else{
		fmt.Println("lose...")
	}

}	

func Vote() bool{
	time.Sleep(time.Duration(rand.Intn(100))*time.Millisecond)
	return rand.Intn(2)==1
}