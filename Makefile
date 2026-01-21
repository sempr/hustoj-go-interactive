.PHONY: all build judge player controller run clean

CONTROLLER = cmd/ctr
JUDGE = cmd/rootfs_judge/bin/judge
PLAYER = cmd/rootfs_player/bin/player

all: build

build: controller judge player

controller:
	go build -o $(CONTROLLER) cmd/contr/main.go

judge:
	g++ -static -o $(JUDGE) cmd/judger/main.cpp

player:
	g++ -static -o $(PLAYER) cmd/player/main.cpp

run: build
	./$(CONTROLLER) \
		-judge-rootfs cmd/rootfs_judge \
		-player-rootfs cmd/rootfs_player \
		-timeout 5000

clean:
	rm -f $(CONTROLLER) $(JUDGE) $(PLAYER)
