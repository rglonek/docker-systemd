set -e
nohup /sbin/init &
apt update
apt install -y wget

wget https://download.aerospike.com/artifacts/aerospike-server-enterprise/6.4.0.7/aerospike-server-enterprise_6.4.0.7_tools-10.0.0_ubuntu22.04_x86_64.tgz
mkdir server
tar -C server -zxvf aerospike-server-enterprise_6.4.0.7_tools-10.0.0_ubuntu22.04_x86_64.tgz
cd server/aerospike*
apt -y install ./*.deb

wget https://github.com/aerospike/aerospike-prometheus-exporter/releases/download/v1.13.0/aerospike-prometheus-exporter_1.13.0_amd64.deb
dpkg -i aerospike-prometheus-exporter_1.13.0_amd64.deb

debconf-set-selections <<< "postfix postfix/mailname string example.com"
debconf-set-selections <<< "postfix postfix/main_mailer_type string 'Internet Site'"
apt install -y postfix mariadb-server apache2
pkill init
