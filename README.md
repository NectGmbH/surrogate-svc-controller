# surrogate-svc-controller
Kubernetes controller which automatically clones services and adds filters for specific labels. Will fallback to non-filtered endpoints.
Use case is e.g. having a dedicated service per availability zone with a fallback to endpoints outside the availability zone.

## Deploy using helm
```
$ helm upgrade -i surrogate-svc-controller --namespace surrogate-svc-controller ./charts/surrogate-svc-controller -f my-values.yaml
```

### Values

| Key       | Default value                               | Description               |
| --------- | ------------------------------------------- | ------------------------- |
| replicas  | 1                                           | Amount of instances    |
| image     | 'kavatech/surrogate-svc-controller:v0.1.0'  | Image of the container    |
| label     | ''                                          | Label for which individuel service subsets should be created (e.g. availabilityzone label)     |
| debug     | false                                       | Whether debug output should be logged     |
| namespace | ''                                          | Namespace in which services should be managed. Empty string means: all namespaces     |
| tag       | ''                                          | Annotation/Label a service should have to be handled. Empty string means: all services     |
| suffixes  | {}                                          | A mapping between the label value and the suffix the matching services should have     |