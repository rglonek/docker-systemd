CC=gcc
CFLAGS=-fPIC

cleanbuild: clean build

build: fork.so fakefork.so

clean:
	rm -f fork.so fakefork.so

fork.so: preload.c
	$(CC) $(CFLAGS) -shared -o $@ $^

fakefork.so: preload_stub.c
	$(CC) $(CFLAGS) -shared -o $@ $^
