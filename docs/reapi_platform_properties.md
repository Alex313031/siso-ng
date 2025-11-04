# RE API platform properties used in Siso

Siso basically just sends platform properties to RE backend
as it is specified in `siso_config["platforms"][platform_ref]`.

There are some platform properties that are specially handled in Siso.

## OSFamily

`OSFamily` is used to determine OS used on RE worker, i.e. filename is
case sensitive or not.
e.g. "Linux", "Windows".

## InputRootAbsolutePath

If the step config has `input_root_absolute_path=true`,
then Siso will set the absolute path of exec root to `InputRootAbsolutePath`.
RE worker will mounts the remote inputs at this path.

## dockerRuntime

If `SISO_EXPERIMENTS=gvisor` is set, then Siso will set
`dockerRuntime=runsc` to enable [gVisor](https://gvisor.dev/).

## dockerChrootPath

`dockerChrootPath=.` will work as chroot mode. i.e. exec root becomes `/`.
Other `dockerChrootPath` value is unsupported.

