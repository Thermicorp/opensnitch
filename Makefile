all: proto daemon/opensnitchd ui

install:
	cd daemon && make install && cd ..
	cd ui && make install && cd ..

proto:
	cd proto && make

ui/resources_rc.py:
	cd ui && make

daemon/opensnitchd:
	cd daemon && make

deps: daemon/opensnitchd ui/resources_rc.py

ui:
	cd ui && make deps

clean:
	cd daemon && make clean
	cd proto && make clean

run:
	cd ui && pip3 install --user --upgrade . && cd ..
	opensnitch-ui --socket unix:///tmp/osui.sock &
	./daemon/opensnitchd -ui-socket unix:///tmp/osui.sock -cpu-profile cpu.profile -mem-profile mem.profile

test: 
	clear 
	make clean
	clear
	mkdir -p rules
	make 
	clear
	make run

adblocker:
	clear 
	make clean
	clear
	make 
	clear
	./make_ads_rules.py
	clear
	cd ui && sudo pip3 install --user --upgrade . && cd ..
	opensnitch-ui --socket unix:///tmp/osui.sock &
	./daemon/opensnitchd -ui-socket unix:///tmp/osui.sock 


