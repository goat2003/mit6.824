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
	ch := make(chan bool)

	for i:=0;i<10;i++{
		go func() {
			ch <- Vote()
		}()
	}
//这里实现并不完美，如果count >= 5了，主线程不会再监听channel，导致其他还在运行的子线程会阻塞在往channel写数据的步骤。但是这里主线程退出后子线程们也会被销毁，影响不大。但如果是在一个长期运行的大型工程中，这里就存在泄露线程leaking threads
	for count<5 && finished!=10{
		vote := <-ch
		if vote{
			count++
		}
		finished++
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