#! /bin/bash

# Delete orphaned jobs

kubectl delete jobs -n k4all-operator-system --selector='app.kubernetes.io/name=k4all-operator'
