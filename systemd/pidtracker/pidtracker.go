package pidtracker

import (
	"log"
	"sync"

	"github.com/bestmethod/inslice"
)

type pids struct {
	sync.RWMutex
	relations map[int][]int
}

func Add(parent, child, infant int) {
	if child == -1 {
		return
	}
	p.Lock()
	defer p.Unlock()
	if parent != -1 {
		p.relations[parent] = append(p.relations[parent], child)
	}
	if infant != -1 {
		p.relations[child] = append(p.relations[child], infant)
	}
}

var p = &pids{
	relations: make(map[int][]int),
}

func Find(pid int) (children []int) {
	p.RLock()
	defer p.RUnlock()
	return find(pid)
}

func find(pid int) (children []int) {
	for parent, child := range p.relations {
		if parent == pid {
			for _, c := range child {
				if !inslice.HasInt(children, c) {
					children = append(children, c)
				}
				for _, cc := range find(c) {
					if !inslice.HasInt(children, cc) {
						children = append(children, cc)
					}
				}
			}
		}
	}
	newchildren := []int{}
	for _, c := range children {
		if inslice.HasInt(p.relations[1], c) {
			newchildren = append(newchildren, c)
		}
	}
	return newchildren
}

func Dump() {
	p.RLock()
	defer p.RUnlock()
	for parent, child := range p.relations {
		log.Printf("%d:%d", parent, child)
	}
}
