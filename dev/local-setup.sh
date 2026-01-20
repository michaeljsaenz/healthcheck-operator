#!/bin/bash
set -e

echo "Creating cluster..."
kind create cluster --config dev/kind-config.yaml

echo "Updating Node DNS..."
docker exec healthcheck-local-control-plane sh -c "echo 'nameserver 8.8.8.8\nnameserver 8.8.4.4\nnameserver 1.1.1.1' > /etc/resolv.conf"
docker exec healthcheck-local-control-plane systemctl restart containerd

echo "Patch Cluster DNS (CoreDNS)..."
kubectl patch configmap coredns -n kube-system --type merge -p '{"data":{"Corefile":".:53 {\n    errors\n    health {\n       lameduck 5s\n    }\n    ready\n    kubernetes cluster.local in-addr.arpa ip6.arpa {\n       pods insecure\n       fallthrough in-addr.arpa ip6.arpa\n       ttl 30\n    }\n    prometheus :9153\n    forward . 8.8.8.8 8.8.4.4 1.1.1.1\n    cache 30\n    loop\n    reload\n    loadbalance\n}\n"}}'

echo "Restarting CoreDNS to apply changes..."
kubectl rollout restart deployment coredns -n kube-system
kubectl rollout status deployment coredns -n kube-system --timeout=60s