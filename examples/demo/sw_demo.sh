#!/usr/bin/env bash

. $(dirname ${BASH_SOURCE})/../../contrib/shell/util.sh

export LC_ALL=en_US.UTF-8

NETWORK="space"
PWD=$(dirname ${BASH_SOURCE})

function cleanup {
	tmux kill-session -t my-session >/dev/null 2>&1
	docker rm -f deathstar luke xwing_luke xwing ship1 2> /dev/null || true
	cilium policy delete root 2> /dev/null
}

trap cleanup EXIT
cleanup

sleep 0.5
desc_rate "A long time ago, in a container cluster far, far away...."
desc_rate ""
desc_rate "It is a period of civil war. The Empire has adopted"
desc_rate "microservices and continuous delivery, despite this,"
desc_rate "Rebel spaceships, striking from a hidden cluster, have"
desc_rate "won their first victory against the evil Galactic Empire."
desc_rate ""
desc_rate "During the battle, Rebel spies managed to steal the"
desc_rate "swagger API specification to the Empire's ultimate weapon,"
desc_rate "the deathstar."
run ""

docker network rm $NETWORK > /dev/null 2>&1
desc_rate "And so it begins..."
run "docker network create --ipv6 --subnet ::1/112 --driver cilium --ipam-driver cilium $NETWORK"

run "docker run -dt --net=$NETWORK --name deathstar -l id.empire.deathstar cilium/starwars"

desc_rate "In order for spaceships to land, the empire establishes"
desc_rate "a network landing policy (L3/L4)."
run "cat sw_policy_l4.json"
run "cilium policy import sw_policy_l4.json"

DEATHSTAR_IP4=$(docker inspect --format '{{ .NetworkSettings.Networks.space.IPAddress }}' deathstar)

run "docker run -dt --net=$NETWORK --name ship1 -l id.spaceship --add-host deathstar:$DEATHSTAR_IP4 tgraf/netperf"
run "cilium endpoint list"
run "docker exec -i ship1 curl -si -XPOST http://deathstar/v1/request-landing"

desc_rate "In the meantime..."
desc_rate ""
run "docker run -dt --net=$NETWORK --name xwing -l id.spaceship --add-host deathstar:$DEATHSTAR_IP4 tgraf/netperf"
run "docker exec -i xwing ping -c 2 deathstar"
run "docker exec -i xwing curl -si -XGET http://deathstar/v1/"
desc_rate "Look at that thermal exhaust port, it seems vulnerable..."
run ""
desc_rate "In the meantime..."
run "cat sw_policy_http.show.json"
run "cilium policy import sw_policy_http.real.json"

desc_rate "The rebels return..."
run "docker exec -i xwing ping -c 2 deathstar"
run "docker exec -i xwing curl -si -XPUT http://deathstar/v1/exhaust-port"

desc_rate "Oh no! The shields are up."
desc_rate "End of demo."
run ""
desc_rate "Here is what you missed..."
desc_rate ""
run "colordiff -Nru sw_policy_http.show.json sw_policy_http.real.json"

run "docker run -dt --net=$NETWORK --name xwing_luke -l id.spaceship --add-host deathstar:$DEATHSTAR_IP4 tgraf/netperf"
run "docker exec -i xwing_luke curl -si -H 'X-Has-Force: true' -XPUT http://deathstar/v1/exhaust-port"
run "docker exec -i xwing_luke ping deathstar"

desc "Cleaning up demo environment"
