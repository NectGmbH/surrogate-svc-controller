package main

import (
    "flag"
    "fmt"
    "strings"
    "time"

    "github.com/sirupsen/logrus"

    "k8s.io/client-go/informers"
    "k8s.io/client-go/kubernetes"
    "k8s.io/client-go/tools/clientcmd"
)

type mapFlags map[string]string

func (m mapFlags) Set(value string) error {
    splitted := strings.Split(value, "=")
    if len(splitted) != 2 {
        return fmt.Errorf("expected key=value but got `%s`", value)
    }

    m[splitted[0]] = splitted[1]

    return nil
}

func (m mapFlags) String() string {
    var str string

    i := 0

    for k, v := range m {
        if i > 0 {
            str += ","
        }

        str += k + "=" + v
        i++
    }

    return str
}

type sliceFlags []string

func (i *sliceFlags) String() string {
    return strings.Join(*i, " ")
}

func (i *sliceFlags) Set(value string) error {
    *i = append(*i, value)
    return nil
}

func main() {
    var kubeconfig string
    var masterURL string
    var label string
    var namespace string
    var debug bool
    var tag string
    suffixes := make(mapFlags)

    flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
    flag.StringVar(&masterURL, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
    flag.StringVar(&label, "label", "", "Pod label for which an surrogate svc should be created")
    flag.StringVar(&namespace, "namespace", "", "Optional, namespace to limit the controller to.")
    flag.BoolVar(&debug, "debug", false, "Enable debug logging")
    flag.StringVar(&tag, "tag", "", "When given, only services with either a label or an annotation where the key matches the tag are considered.")
    flag.Var(&suffixes, "suffix", "Service suffixes to use for the individual label values. Expected format is -suffix labelValueA=foo -suffix labelValueB=foo")
    flag.Parse()

    if debug {
        logrus.SetLevel(logrus.DebugLevel)
    }

    if label == "" {
        logrus.Fatalf("Missing -label option, please specifiy the label for which surrogate services should be created.")
    }

    if len(suffixes) == 0 {
        logrus.Fatalf("Missing -suffix option, please specifiy at least one service name suffix. Expected format is -suffix labelValueA=foo -suffix labelValueB=foo")
    }

    cfg, err := clientcmd.BuildConfigFromFlags(masterURL, kubeconfig)
    if err != nil {
        logrus.Fatalf("Error building kubeconfig: %s", err.Error())
    }

    kubeClient, err := kubernetes.NewForConfig(cfg)
    if err != nil {
        logrus.Fatalf("Error building kubernetes clientset: %s", err.Error())
    }

    var kubeInformerFactory informers.SharedInformerFactory

    if namespace == "" {
        kubeInformerFactory = informers.NewSharedInformerFactory(kubeClient, time.Second*30)
    } else {
        kubeInformerFactory = informers.NewSharedInformerFactoryWithOptions(kubeClient, time.Second*30, informers.WithNamespace(namespace))
    }

    controller := NewController(
        kubeClient,
        kubeInformerFactory.Core().V1().Services(),
        kubeInformerFactory.Core().V1().Endpoints(),
        suffixes,
        label,
        tag,
    )

    stopCh := make(chan struct{})
    defer close(stopCh)

    kubeInformerFactory.Start(stopCh)

    controller.Run(2, stopCh)
    logrus.Fatalf("controller stopped")
}
