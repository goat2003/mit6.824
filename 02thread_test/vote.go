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

	for i:=0;i<10;i++{
		go func() {
			vote := Vote()
			mu.Lock()
			defer mu.Unlock()
			if vote{
				count++
			}
			finished++
		}()
	}

	for count<5 && finished!=10{
		//wait
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