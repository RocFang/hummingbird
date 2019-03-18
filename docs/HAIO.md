# Hummingbird All In One (HAIO)

To get started developing Hummingbird you need to have Ubuntu 16.04 and then download the hummingbird executable from the [latest build](https://troubling.github.io/hummingbird/bin/hummingbird) to the system, run `./hummingbird init haio`, and finally run the resulting `./hummingbird-init-haio.sh`. This will install, download, and configure everything needed to start writing and testing Hummingbird code.

You may want to setup sudo to allow you to switch without entering a password; less secure, but maybe fine for a privately contained virtual machine:

```sh
echo "$USER ALL=(ALL) NOPASSWD: ALL" | sudo tee -a /etc/sudoers
```

Once done, the `hummingbird-init-haio.sh` will have placed all the Hummingbird code at ~/go/src/github.com/RocFang/hummingbird -- the git origin remote will be set to the official fork. You can patch this up with `git remote set-url origin git@github.com:YOU/hummingbird` and perhaps add `git remote add upstream git@github.com:troubling/hummingbird` if desired.

A normal development loop would be to write code and then:

```sh
make haio
hbreset        # optionally, throws away all previously stored data
hbmain start   # or hball start
# test changes
```

Note that the systemd services, run by hbmain and hball, will use /usr/local/bin/hummingbird which is installed by `make haio`.

There a few other make targets as well:

```sh
make                 # just does quick compile, mostly just a syntax check
make fmt             # runs go fmt with the options we like
make test            # runs go vet and the unit tests with coverage
make functional-test # runs the functional tests; cluster must be running
```

Logs will be going through the standard systemd log system, so if you're used to journalctl you can just use that.  
But, hblog is also provided in case that's simpler:

```sh
hblog proxy    # shows all the log lines from the proxy server
hblog proxy -f # shows recent log lines and follows, like a tail -f
hblog object1  # shows logs for just the first object server
hblog object\* # shows logs for all the object servers (can also have -f)
```

You can run Openstack Swift's functional tests against Hummingbird if you want:

```sh
hbswifttests
```

If you want to stop/start individual services, you would do it much like a "real" user would:

```
sudo systemctl stop hummingbird-proxy
```

Although, since an HAIO pretends to be a cluster of 4 machines, the account, container, and object services all have 4 each:

```
sudo systemctl restart hummingbird-object1
```

Finally, there are two more commands that you may find useful occasionally:

```
hbrings     # rebuilds the rings from scratch
hbmount     # mounts /srv/hb -- usually needed after a reboot
```
