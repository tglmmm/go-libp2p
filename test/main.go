package main

import (
	"context"
	"fmt"
	"time"
)

type testA struct {
	ctx context.Context
}

func main() {
	timer := time.NewTimer(1 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			fmt.Println("ok")
			timer.Reset(1 * time.Second)
		default:
			time.Sleep(1 * time.Second)
			fmt.Println("default")
		}
	}

}

func gA() {
	go background()
}

func background() {
	for {
		time.Sleep(1 * time.Second)
		fmt.Println("sleep。。。")
	}
}
