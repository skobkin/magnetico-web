<?php

namespace App\Validator\Constraints;

use App\Entity\Invite;
use App\Repository\InviteRepository;
use Symfony\Component\Validator\{Constraint, ConstraintValidator};

class ValidInviteValidator extends ConstraintValidator
{
    /** @var InviteRepository */
    private $inviteRepo;

    public function __construct(InviteRepository $inviteRepo)
    {
        $this->inviteRepo = $inviteRepo;
    }

    /**
     * @param mixed $value
     * @param ValidInvite $constraint
     */
    public function validate($value, Constraint $constraint)
    {
        /** @var Invite $invite */
        if (null === $invite = $this->inviteRepo->findOneBy(['code' => $value])) {
            $this->context->buildViolation($constraint->notFoundMessage)
                ->setParameter('{{ code }}', $value)
                ->addViolation()
            ;

            return;
        }

        if (null !== $invite->getUsedBy()) {
            $this->context->buildViolation($constraint->usedMessage)
                ->setParameter('{{ code }}', $value)
                ->addViolation()
            ;
        }
    }
}