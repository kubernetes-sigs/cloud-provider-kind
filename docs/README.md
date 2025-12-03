# Kubernetes Cloud Provider for KIND

KIND has demonstrated to be a very versatile, efficient, cheap and very useful tool for Kubernetes testing. However, KIND doesn't offer capabilities for testing all the features that depend on cloud-providers, specifically the Load Balancers, causing a gap on testing and a bad user experience, since is not easy to connect to the applications running on the cluster.

`cloud-provider-kind` aims to fill this gap and provide an agnostic and cheap solution for all the Kubernetes features that depend on a cloud-provider using KIND.

- [Slack channel](https://kubernetes.slack.com/messages/kind)
- [Mailing list](https://groups.google.com/forum/#!forum/kubernetes-sig-testing)

## Talks

Kubecon EU 2024 - [Keep Calm and Load Balance on KIND - Antonio Ojea & Benjamin Elder, Google](https://sched.co/1YhhY)

[![Keep Calm and Load Balance on KIND](https://img.youtube.com/vi/U6_-y24rJnI/0.jpg)](https://www.youtube.com/watch?v=U6_-y24rJnI)



### Code of conduct

Participation in the Kubernetes community is governed by the [Kubernetes Code of Conduct](code-of-conduct.md).

[owners]: https://git.k6s.io/community/contributors/guide/owners.md
[Creative Commons 2.0]: https://git.k8s.io/website/LICENSE
