package main

import (
	"fmt"
	"github.com/AnishPrakash/arena/internal/perfcount"
)

func main() {
	ok, why := perfcount.Available()
	if ok {
		fmt.Println("PMU AVAILABLE - instruction counting is possible on this machine")
	} else {
		fmt.Println("PMU UNAVAILABLE:", why)
	}
}
