#!/usr/bin/env python3
# vim:fileencoding=utf-8

from os import listdir, stat, rename, getenv
from os.path import isfile, join
from datetime import datetime

def filter_log(p):
    def f(x):
        if not (isinstance(p, str) and isinstance(x, str)):
            return False

        # skip directories
        full_path = join(p, x)
        if not isfile(full_path):
            return False

        # skip archived logs
        idx = x.split('.')[-1]
        if not idx.isnumeric():
            return False

        if int(idx) > 20010101:
            return False

        return True
    return f
    

if __name__ == '__main__':
    LOG_PATH = getenv('LOG_PATH', None)
    if LOG_PATH is None:
        print('LOG_PATH is not set, failed to clean up log dir')
        exit(-1)

    files = filter(filter_log(LOG_PATH), listdir(LOG_PATH))

    for f in files:
        path = join(LOG_PATH, f)
        ctime = stat(path).st_ctime
        dt = datetime.fromtimestamp(ctime)
        arname = f"{path}.{dt.strftime('%y%m%d%H%M%S')}"
        rename(path, arname)
        print(f'renamed {path} to {arname}')

