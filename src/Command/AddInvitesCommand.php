<?php
declare(strict_types=1);

namespace App\Command;

use App\Repository\UserRepository;
use App\User\InviteManager;
use Doctrine\ORM\EntityManagerInterface;
use Symfony\Component\Console\Command\Command;
use Symfony\Component\Console\Input\{InputArgument, InputInterface};
use Symfony\Component\Console\Output\OutputInterface;

class AddInvitesCommand extends Command
{
    public function __construct(
        private readonly EntityManagerInterface $em,
        private readonly UserRepository $userRepo,
        private readonly InviteManager $inviteManager
    ) {
        parent::__construct();
    }

    protected function configure()
    {
        $this
            ->setName('invite:add')
            ->addArgument('username', InputArgument::REQUIRED, 'Username')
            ->addArgument('number', InputArgument::REQUIRED, 'Number of invites')
        ;
    }

    protected function execute(InputInterface $input, OutputInterface $output): int
    {
        $username = $input->getArgument('username');
        $number = (int) $input->getArgument('number');

        if (null === $user = $this->userRepo->findOneBy(['username' => $username])) {
            $output->writeln('<error>User not found.</error>');

            return Command::FAILURE;
        }

        $this->inviteManager->createInvitesForUser($user, $number);

        $this->em->flush();

        $output->writeln(sprintf('<info>%d invites added to \'%s\'.</info>', $number, $user->getUserIdentifier()));

        return Command::SUCCESS;
    }
}
