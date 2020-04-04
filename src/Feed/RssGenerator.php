<?php


namespace App\Feed;

use App\Magnet\MagnetGenerator;
use App\Magnetico\Entity\Torrent;
use App\Magnetico\Repository\TorrentRepository;
use Doctrine\ORM\QueryBuilder;
use Suin\RSSWriter\{Channel, Feed, Item};
use Symfony\Component\Routing\{Generator\UrlGeneratorInterface, RouterInterface};

/**
 * Generates RSS feed.
 *
 * @see https://www.bittorrent.org/beps/bep_0036.html
 */
class RssGenerator
{
    private const PER_PAGE = 1000;
    private const MIME_TYPE = 'application/x-bittorrent';

    private TorrentRepository $repo;
    private RouterInterface $router;
    private MagnetGenerator $magnetGenerator;

    public function __construct(TorrentRepository $repo, RouterInterface $router, MagnetGenerator $magnetGenerator)
    {
        $this->repo = $repo;
        $this->router = $router;
        $this->magnetGenerator = $magnetGenerator;
    }

    public function generateLast(int $page): string
    {
        $qb = $this->createLastTorrentsQueryBuilder(self::PER_PAGE, $page * self::PER_PAGE);

        $feed = $this->createFeedFromTorrents($qb->getQuery()->getResult());

        return $feed->render();
    }

    /**
     * @param Torrent[] $torrents
     *
     * @return Feed
     */
    private function createFeedFromTorrents(array $torrents): Feed
    {
        $feed = new Feed();
        $channel = $this->createChannel();
        $feed->addChannel($channel);

        foreach ($this->createItemsFromTorrents($torrents) as $item) {
            $channel->addItem($item);
        }

        // TODO feed pagination

        return $feed;
    }

    private function createChannel(): Channel
    {
        $time = time();

        $channel = new Channel();
        $channel
            ->title('Last')
            ->description('Last torrents')
            ->url($this->generateUrl('index'))
            ->feedUrl($this->generateUrl('api_v1_rss_last'))
            ->language('en-US')
            ->pubDate($time)
            ->lastBuildDate($time)
            ->ttl(15)
        ;

        return $channel;
    }

    /**
     * @param Torrent[] $torrents
     *
     * @return Item[]
     */
    private function createItemsFromTorrents(array $torrents): array
    {
        $items = [];

        foreach ($torrents as $torrent) {
            $items[] = $this->createItemFromTorrent($torrent);
        }

        return $items;
    }

    private function createItemFromTorrent(Torrent $torrent): Item
    {
        $item = new Item();
        $item
            ->title($torrent->getName())
            ->description($torrent->getInfoHash())
            ->url($this->generateUrl('torrents_show', ['id' => $torrent->getId()]))
            ->enclosure(
                $this->magnetGenerator->generate($torrent->getInfoHash(), $torrent->getName()),
                $torrent->getTotalSize(),
                self::MIME_TYPE
            )
            ->guid($torrent->getInfoHash())
        ;

        return $item;
    }

    private function createLastTorrentsQueryBuilder(int $limit = self::PER_PAGE, int $offset = 0): QueryBuilder
    {
        $qb = $this->repo->createQueryBuilder('t');
        $qb
            ->select('t')
            ->orderBy('t.id', 'DESC')
            ->setMaxResults($limit)
            ->setFirstResult($offset)
        ;

        return $qb;
    }

    private function generateUrl(string $route, array $parameters = []): string
    {
        return $this->router->generate($route, $parameters, UrlGeneratorInterface::ABSOLUTE_URL);
    }
}