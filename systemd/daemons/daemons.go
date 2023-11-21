package daemons

import (
	"docker-systemd/common"
	"log"
	"os"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/bestmethod/inslice"
)

type daemons struct {
	list map[string]*daemon
	sync.RWMutex
}

func (ds *daemons) List() []string {
	l := []string{}
	ds.RLock()
	defer ds.RUnlock()
	for i := range ds.list {
		l = append(l, i)
	}
	sort.Strings(l)
	return l
}

func (ds *daemons) LoadAndStart() error {
	err := ds.Reload()
	if err != nil {
		return err
	}
	target := "/etc/systemd/system/multi-user.target.wants"
	dir, err := os.ReadDir(target)
	if err != nil {
		return nil
	}
	ds.RLock()
	for _, entry := range dir {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".service") {
			continue
		}
		serviceName := strings.TrimSuffix(entry.Name(), ".service")
		d, ok := ds.list[serviceName]
		if !ok {
			log.Printf("INIT: Wanted target service not found: %s", serviceName)
		}
		log.Printf("INIT: Starting: %s", serviceName)
		err = d.Start()
		if err != nil {
			log.Printf("INIT: Failed to start %s: %s", serviceName, err)
		} else {
			log.Printf("INIT: Started: %s", serviceName)
		}
	}
	ds.RUnlock()
	return nil
}

func (ds *daemons) StopAll() error {
	ds.RLock()
	for _, item := range ds.list {
		state := item.State()
		if state != StateStopped && state != StateStopping {
			log.Printf("SHUTDOWN: Stopping: %s", item.Name())
			err := item.Stop()
			if err != nil {
				log.Printf("SHUTDOWN: Failed to stop %s: %s", item.Name(), err)
			} else {
				log.Printf("SHUTDOWN: Stopped: %s", item.Name())
			}
		}
	}
	ds.RUnlock()
	return nil
}

func (ds *daemons) Reload() error {
	ds.RWMutex.Lock()
	for _, d := range ds.list {
		d.olddef = d.def
		d.def = nil
	}
	defer ds.RWMutex.Unlock()
	failedloads := []string{}
	processedFiles := []string{}
	for _, locs := range common.GetSystemdPaths() {
		if _, err := os.Stat(locs); err != nil {
			continue
		}
		entries, err := os.ReadDir(locs)
		if err != nil {
			log.Printf("Could not read %s: %s", locs, err)
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			fn := entry.Name()
			fpath := path.Join(locs, fn)
			if !strings.HasSuffix(fpath, ".service") {
				continue
			}
			fn = strings.TrimSuffix(fn, ".service")
			d := &daemon{
				name:  fn,
				paths: []string{fpath},
				state: StateStopped,
			}
			if _, ok := ds.list[fn]; ok {
				d = ds.list[fn]
				d.Lock()
				if !inslice.HasString(d.paths, fpath) {
					d.paths = append(d.paths, fpath)
				}
				d.Unlock()
			}
			// handle masked services
			isMasked := false
			d.Lock()
			d.isMasked = false
			d.Unlock()
			processedFile := fpath
			if nstat, err := os.Lstat(fpath); err == nil && nstat.Mode()&os.ModeSymlink != 0 {
				linkdest, err := os.Readlink(fpath)
				if err == nil {
					processedFile = linkdest
				}
				if err == nil && linkdest == "/dev/null" {
					d.Lock()
					d.isMasked = true
					isMasked = true
					d.Unlock()
				}
			}
			// end
			if !isMasked && !inslice.HasString(processedFiles, processedFile) {
				processedFiles = append(processedFiles, processedFile)
				f, err := os.Open(fpath)
				if err != nil {
					log.Printf("Could not read %s: %s", fpath, err)
					continue
				}
				defer f.Close()
				err = loadUnitFile(d, f)
				if err != nil {
					d.Lock()
					d.def = d.olddef
					d.olddef = nil
					failedloads = append(failedloads, d.name)
					d.Unlock()
					log.Printf("ERROR loading unit for %s: %s", fpath, err)
					continue
				}
			}
			ds.list[fn] = d
		}
	}
	for _, locs := range common.GetSystemdPaths() {
		if _, err := os.Stat(locs); err != nil {
			continue
		}
		entries, err := os.ReadDir(locs)
		if err != nil {
			log.Printf("Could not read %s: %s", locs, err)
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			fn := entry.Name()
			fpath := path.Join(locs, fn)
			if !strings.HasSuffix(fpath, ".service.d") {
				continue
			}
			fn = strings.TrimSuffix(fn, ".service.d")
			if inslice.HasString(failedloads, fn) {
				continue
			}
			if _, ok := ds.list[fn]; !ok {
				continue
			}
			d := ds.list[fn]
			// now load all files from the file subdir
			entriesa, err := os.ReadDir(fpath)
			if err != nil {
				log.Printf("Could not read %s: %s", fpath, err)
				continue
			}
			for _, entrya := range entriesa {
				if entrya.IsDir() {
					continue
				}
				fna := entrya.Name()
				if !strings.HasSuffix(fna, ".conf") {
					continue
				}
				fpatha := path.Join(fpath, fna)
				d.Lock()
				if !inslice.HasString(d.paths, fpatha) {
					d.paths = append(d.paths, fpatha)
				}
				d.Unlock()
				processedFile := fpatha
				if nstat, err := os.Lstat(fpatha); err == nil && nstat.Mode()&os.ModeSymlink != 0 {
					linkdest, err := os.Readlink(fpatha)
					if err == nil {
						processedFile = linkdest
					}
				}
				if !inslice.HasString(processedFiles, processedFile) {
					processedFiles = append(processedFiles, processedFile)
					f, err := os.Open(fpatha)
					if err != nil {
						log.Printf("Could not read %s: %s", fpatha, err)
						continue
					}
					defer f.Close()
					err = loadUnitFile(d, f)
					if err != nil {
						d.Lock()
						d.def = d.olddef
						d.olddef = nil
						failedloads = append(failedloads, d.name)
						d.Unlock()
						log.Printf("ERROR loading unit for %s: %s", fpatha, err)
						continue
					}
				}
			}
			// end
			ds.list[fn] = d
		}
	}
	for r, d := range ds.list {
		d.Lock()
		d.olddef = nil
		if d.def == nil && d.state == StateStopped {
			delete(ds.list, r)
		}
		d.Unlock()
	}
	for r, d := range ds.list {
		d.Lock()
		for depName := range d.def.Requires {
			d.def.Requires[depName] = ds.list[depName]
			if _, ok := ds.list[depName]; !ok {
				continue
			}
			ds.list[depName].Lock()
			ds.list[depName].def.RequiredBy[r] = d
			ds.list[depName].Unlock()
		}
		for depName := range d.def.RequiredBy {
			d.def.RequiredBy[depName] = ds.list[depName]
			if _, ok := ds.list[depName]; !ok {
				continue
			}
			ds.list[depName].Lock()
			ds.list[depName].def.Requires[r] = d
			ds.list[depName].Unlock()
		}
		for depName := range d.def.Wants {
			d.def.Wants[depName] = ds.list[depName]
			if _, ok := ds.list[depName]; !ok {
				continue
			}
			ds.list[depName].Lock()
			ds.list[depName].def.WantedBy[r] = d
			ds.list[depName].Unlock()
		}
		for depName := range d.def.WantedBy {
			d.def.WantedBy[depName] = ds.list[depName]
			if _, ok := ds.list[depName]; !ok {
				continue
			}
			ds.list[depName].Lock()
			ds.list[depName].def.Wants[r] = d
			ds.list[depName].Unlock()
		}
		for depName := range d.def.Requisite {
			d.def.Requires[depName] = ds.list[depName]
			if _, ok := ds.list[depName]; !ok {
				continue
			}
			ds.list[depName].Lock()
			ds.list[depName].def.RequisiteOf[r] = d
			ds.list[depName].Unlock()
		}
		for depName := range d.def.RequisiteOf {
			d.def.RequisiteOf[depName] = ds.list[depName]
			if _, ok := ds.list[depName]; !ok {
				continue
			}
			ds.list[depName].Lock()
			ds.list[depName].def.Requisite[r] = d
			ds.list[depName].Unlock()
		}
		for depName := range d.def.BindsTo {
			d.def.BindsTo[depName] = ds.list[depName]
			if _, ok := ds.list[depName]; !ok {
				continue
			}
			ds.list[depName].Lock()
			ds.list[depName].def.BoundBy[r] = d
			ds.list[depName].Unlock()
		}
		for depName := range d.def.BoundBy {
			d.def.BoundBy[depName] = ds.list[depName]
			if _, ok := ds.list[depName]; !ok {
				continue
			}
			ds.list[depName].Lock()
			ds.list[depName].def.BindsTo[r] = d
			ds.list[depName].Unlock()
		}
		for depName := range d.def.PartOf {
			d.def.PartOf[depName] = ds.list[depName]
			if _, ok := ds.list[depName]; !ok {
				continue
			}
			ds.list[depName].Lock()
			ds.list[depName].def.ConsistsOf[r] = d
			ds.list[depName].Unlock()
		}
		for depName := range d.def.ConsistsOf {
			d.def.ConsistsOf[depName] = ds.list[depName]
			if _, ok := ds.list[depName]; !ok {
				continue
			}
			ds.list[depName].Lock()
			ds.list[depName].def.PartOf[r] = d
			ds.list[depName].Unlock()
		}
		for depName := range d.def.Upholds {
			d.def.Upholds[depName] = ds.list[depName]
			if _, ok := ds.list[depName]; !ok {
				continue
			}
			ds.list[depName].Lock()
			ds.list[depName].def.UpheldBy[r] = d
			ds.list[depName].Unlock()
		}
		for depName := range d.def.UpheldBy {
			d.def.UpheldBy[depName] = ds.list[depName]
			if _, ok := ds.list[depName]; !ok {
				continue
			}
			ds.list[depName].Lock()
			ds.list[depName].def.Upholds[r] = d
			ds.list[depName].Unlock()
		}
		for depName := range d.def.Conflicts {
			d.def.Conflicts[depName] = ds.list[depName]
			if _, ok := ds.list[depName]; !ok {
				continue
			}
			ds.list[depName].Lock()
			ds.list[depName].def.ConflictedBy[r] = d
			ds.list[depName].Unlock()
		}
		for depName := range d.def.ConflictedBy {
			d.def.ConflictedBy[depName] = ds.list[depName]
			if _, ok := ds.list[depName]; !ok {
				continue
			}
			ds.list[depName].Lock()
			ds.list[depName].def.Conflicts[r] = d
			ds.list[depName].Unlock()
		}
		for depName := range d.def.Before {
			d.def.Before[depName] = ds.list[depName]
			if _, ok := ds.list[depName]; !ok {
				continue
			}
			ds.list[depName].Lock()
			ds.list[depName].def.After[r] = d
			ds.list[depName].Unlock()
		}
		for depName := range d.def.After {
			d.def.After[depName] = ds.list[depName]
			if _, ok := ds.list[depName]; !ok {
				continue
			}
			ds.list[depName].Lock()
			ds.list[depName].def.Before[r] = d
			ds.list[depName].Unlock()
		}
		for depName := range d.def.OnFailure {
			d.def.OnFailure[depName] = ds.list[depName]
			if _, ok := ds.list[depName]; !ok {
				continue
			}
			ds.list[depName].Lock()
			ds.list[depName].def.OnSuccess[r] = d
			ds.list[depName].Unlock()
		}
		for depName := range d.def.OnSuccess {
			d.def.OnSuccess[depName] = ds.list[depName]
			if _, ok := ds.list[depName]; !ok {
				continue
			}
			ds.list[depName].Lock()
			ds.list[depName].def.OnFailure[r] = d
			ds.list[depName].Unlock()
		}
		d.Unlock()
	}
	return nil
}

func (ds *daemons) Find(name string) (Daemon, error) {
	ds.RLock()
	defer ds.RUnlock()
	if d, ok := ds.list[name]; ok {
		return d, nil
	}
	ds.RUnlock()
	err := ds.Reload()
	ds.RLock()
	if err != nil {
		return nil, err
	}
	if d, ok := ds.list[name]; ok {
		return d, nil
	}
	return nil, ErrNotFound
}
