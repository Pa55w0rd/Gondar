package hostd

func (j *Job) installBinDir() string {
	if j.binDir != "" {
		return j.binDir
	}
	return binDir
}
