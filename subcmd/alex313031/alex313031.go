package alex313031

var use_custom_ng bool = true;

func IsNG() (bool) {
  var retval bool = use_custom_ng
  return retval
}

func init() {
  IsNG()
}

func GetCustomizations() (string) {
  if IsNG() {
    return "siso-ng"
  } else {
    return "siso"
  }
}
